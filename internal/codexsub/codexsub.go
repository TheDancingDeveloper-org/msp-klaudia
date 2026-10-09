// Package codexsub runs a Codex child in an isolated git worktree and adopts
// its changes back, the phase-0 stand-in for a native per-agent worktree
// (WI-1134, which only happens if this proves insufficient).
//
// The child is `codex exec`, not a thread inside Klaudia's own agent graph, so
// it has its own approvals and its own context. What it shares with a Klaudia
// sub-agent is the checkout: [worktree.New] seeds one from the parent's
// uncommitted state, and [worktree.Adopt] brings the result back with a
// per-repository lock and a conflict report.
package codexsub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/greenthread-ai/klaudia/internal/worktree"
)

// sessionIDSupported reports whether the installed codex accepts --session-id.
//
// TODO(WI-1126): the fork patch that threads ThreadStartParams.thread_id into
// reserved_thread_id has not landed, so codex exec rejects --session-id today.
// Until it has, the flag is never passed. Once `codex exec --help` names it,
// this becomes true and the flag is added. Check rather than assume: a binary
// built from the fork and one built from upstream can both be called `codex`.
func sessionIDSupported(bin string) bool {
	out, err := exec.Command(bin, "exec", "--help").CombinedOutput()
	if err != nil && len(out) == 0 {
		return false
	}
	return strings.Contains(string(out), "--session-id")
}

// Request is one spawn.
type Request struct {
	// Task is the prompt handed to codex exec.
	Task string `json:"task"`
	// WorkingDir is the repository the child works in. It must be inside a git
	// checkout; with Isolation "worktree" it must be the checkout's top level,
	// because that is where the worktree is cut from.
	WorkingDir string `json:"working_dir"`
	// Isolation is "worktree" (default: a seeded checkout, adopted back) or
	// "shared" (run directly in WorkingDir, nothing adopted).
	Isolation string `json:"isolation,omitempty"`
	// OutputSchema, when set, is a JSON Schema the child's final message must
	// satisfy. A mismatch is an error and, for a worktree, nothing is adopted:
	// a result the caller cannot use should not change their tree.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	// Background starts the child and returns a job id instead of waiting.
	Background bool `json:"background,omitempty"`
	// Job, when set, does not spawn anything: it reads the job of that id.
	// Wait blocks until the job finishes; otherwise the call returns its state.
	Job  string `json:"job,omitempty"`
	Wait bool   `json:"wait,omitempty"`
}

// Result is what a spawn produced.
type Result struct {
	// Final is the child's last message, schema-validated when a schema was given.
	Final string `json:"final,omitempty"`
	// Adopted and Conflicted are the paths worktree.Adopt reported. Both are
	// absent for a shared run, which changes WorkingDir directly.
	Adopted    []string `json:"adopted,omitempty"`
	Conflicted []string `json:"conflicted,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	// Job is set when the spawn was backgrounded, or when this result is a poll.
	Job string `json:"job,omitempty"`
	// State is "running", "done" or "failed" for a background job.
	State string `json:"state,omitempty"`
	// Error is why a finished job failed. A failed job is still a successful
	// tool call: the caller asked to be told, and an MCP error would hide the
	// adopt report that says what came back anyway.
	Error string `json:"error,omitempty"`
}

const (
	stateRunning = "running"
	stateDone    = "done"
	stateFailed  = "failed"
)

// Runner executes children. Tests substitute one; the zero value runs codex.
type Runner struct {
	// Bin is the codex executable. Empty means "codex" on PATH.
	Bin string
	// Now names jobs. Tests pin it.
	Now func() time.Time

	mu   sync.Mutex
	jobs map[string]*job
	seq  int
}

type job struct {
	id     string
	done   chan struct{}
	result Result
	err    string
}

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "codex"
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run validates req and either starts a child or reads one already running.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if req.Job != "" {
		return r.poll(ctx, req.Job, req.Wait)
	}
	if strings.TrimSpace(req.Task) == "" {
		return Result{}, errors.New("task is empty")
	}
	dir, err := validateDir(req.WorkingDir)
	if err != nil {
		return Result{}, err
	}
	switch req.Isolation {
	case "", "worktree", "shared":
	default:
		return Result{}, fmt.Errorf("isolation %q is not worktree or shared", req.Isolation)
	}
	if len(req.OutputSchema) > 0 && !json.Valid(req.OutputSchema) {
		return Result{}, errors.New("output_schema is not valid JSON")
	}
	if req.Background {
		return r.start(ctx, req, dir), nil
	}
	res, err := r.execute(ctx, req, dir)
	if err != nil {
		return Result{}, err
	}
	res.State = stateDone
	return res, nil
}

func validateDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("working_dir is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("working_dir: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("working_dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working_dir %s is not a directory", abs)
	}
	return abs, nil
}

func (r *Runner) start(ctx context.Context, req Request, dir string) Result {
	r.mu.Lock()
	r.seq++
	id := fmt.Sprintf("codexsub-%s-%d", r.now().UTC().Format("20060102-150405"), r.seq)
	j := &job{id: id, done: make(chan struct{})}
	if r.jobs == nil {
		r.jobs = map[string]*job{}
	}
	r.jobs[id] = j
	r.mu.Unlock()

	// The job outlives the tool call that started it, so it keeps its own
	// context rather than the request's, which the server cancels on return.
	go func() {
		defer close(j.done)
		res, err := r.execute(context.WithoutCancel(ctx), req, dir)
		r.mu.Lock()
		defer r.mu.Unlock()
		if err != nil {
			j.err = err.Error()
			j.result.State = stateFailed
			return
		}
		res.State = stateDone
		res.Job = id
		j.result = res
	}()
	return Result{Job: id, State: stateRunning}
}

func (r *Runner) poll(ctx context.Context, id string, wait bool) (Result, error) {
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j == nil {
		return Result{}, fmt.Errorf("no job %q", id)
	}
	if wait {
		select {
		case <-j.done:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	select {
	case <-j.done:
	default:
		return Result{Job: id, State: stateRunning}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if j.err != "" {
		return Result{Job: id, State: stateFailed, Error: j.err}, nil
	}
	return j.result, nil
}

func (r *Runner) execute(ctx context.Context, req Request, dir string) (Result, error) {
	work := dir
	var tree *worktree.Tree
	if req.Isolation != "shared" {
		var err error
		tree, err = worktree.New(ctx, dir, "codex")
		if err != nil {
			return Result{}, fmt.Errorf("seeding worktree: %w", err)
		}
		work = tree.Dir
		defer tree.Remove(ctx) //nolint:errcheck // a leaked checkout is pruned later
	}

	final, err := r.codex(ctx, work, req)
	if err != nil {
		return Result{}, err
	}
	if verr := validate(req.OutputSchema, final); verr != nil {
		return Result{}, fmt.Errorf("final message does not match output_schema: %w", verr)
	}

	res := Result{Final: final}
	if tree != nil {
		rep, aerr := tree.Adopt(ctx)
		if aerr != nil {
			return Result{}, fmt.Errorf("adopting changes: %w", aerr)
		}
		res.Adopted = rep.Adopted
		res.Conflicted = rep.Conflicted
		res.Summary = rep.Summary()
	}
	return res, nil
}

// codex runs one child and reads its final message.
//
// --json prints an event per line; the last {"type":"item.completed"} whose
// item is an agent message is the answer. -o writes the same answer to a file,
// which is the fallback when the event stream has no such item — a child that
// exits 0 having said nothing is reported as empty, not as success-shaped
// silence the caller cannot tell from a real answer. --ephemeral keeps the
// child out of the user's session list: it was spawned, not started by them.
func (r *Runner) codex(ctx context.Context, dir string, req Request) (string, error) {
	bin := r.bin()
	tmp, err := os.MkdirTemp("", "codexsub-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	last := filepath.Join(tmp, "last.txt")

	args := []string{"exec", "--cd", dir, "--json", "--ephemeral", "-o", last}
	if len(req.OutputSchema) > 0 {
		schema := filepath.Join(tmp, "schema.json")
		if werr := os.WriteFile(schema, req.OutputSchema, 0o600); werr != nil {
			return "", werr
		}
		args = append(args, "--output-schema", schema)
	}
	// TODO(WI-1126): pass --session-id once the fork accepts it. See sessionIDSupported.
	if sessionIDSupported(bin) {
		args = append(args, "--session-id", filepath.Base(dir))
	}
	args = append(args, req.Task)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CODEX_NONINTERACTIVE=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	final := finalFromEvents(stdout.Bytes())
	if final == "" {
		if b, rerr := os.ReadFile(last); rerr == nil {
			final = strings.TrimSpace(string(b))
		}
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return "", fmt.Errorf("codex exec: %s", msg)
	}
	return final, nil
}

// finalFromEvents scans codex's --json stream for the last agent message.
//
// The stream is a sequence of events, one JSON object per line. Only an
// item.completed whose item is an agent message carries the answer; reasoning
// items and tool calls are the child's own trace and not what the caller
// asked for. A line that is not JSON is skipped: codex warns on stderr, but a
// build that leaks a line onto stdout should not sink the whole result.
func finalFromEvents(out []byte) string {
	var last string
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "item.completed" && ev.Item.Type == "agent_message" && ev.Item.Text != "" {
			last = ev.Item.Text
		}
	}
	return strings.TrimSpace(last)
}

// validate checks final against schema. An empty schema accepts anything.
//
// The schema arrives from the caller, which is a model, so it is compiled with
// the same restriction internal/schema uses: no $ref out of the document. A
// schema of {"$ref":"file:///etc/hostname"} must not read that file.
func validate(schema json.RawMessage, final string) error {
	if len(schema) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return fmt.Errorf("output_schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{
		"file":  refuseExternal{},
		"http":  refuseExternal{},
		"https": refuseExternal{},
	})
	const id = "mem://codexsub.json"
	if err := c.AddResource(id, doc); err != nil {
		return fmt.Errorf("output_schema: %w", err)
	}
	compiled, err := c.Compile(id)
	if err != nil {
		return fmt.Errorf("output_schema: %w", err)
	}
	var inst any
	if err := json.Unmarshal([]byte(final), &inst); err != nil {
		return fmt.Errorf("final message is not JSON: %w", err)
	}
	return compiled.Validate(inst)
}

type refuseExternal struct{}

func (refuseExternal) Load(url string) (any, error) {
	return nil, fmt.Errorf("output_schema cannot reference %s", url)
}
