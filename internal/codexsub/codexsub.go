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

	"github.com/google/uuid"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/greenthread-ai/klaudia/internal/worktree"
)

// DepthEnv is the environment variable the server sets in the child's
// environment and reads from its own. A child that calls spawn_isolated again
// runs this server at depth+1, and the default ceiling refuses that.
const DepthEnv = "KLAUDIA_CODEXSUB_DEPTH"

// sessionIDSupported reports whether the installed codex accepts --session-id.
//
// TODO(WI-1126): the fork patch that threads ThreadStartParams.thread_id into
// reserved_thread_id has not landed, so codex exec rejects --session-id today.
// Until it has, the flag is never passed. Once `codex exec --help` names it,
// this becomes true and the flag is added with a fresh UUID. Check rather
// than assume: a binary built from the fork and one built from upstream can
// both be called `codex`.
func sessionIDSupported(bin string) bool {
	out, err := exec.Command(bin, "exec", "--help").CombinedOutput()
	if err != nil && len(out) == 0 {
		return false
	}
	return strings.Contains(string(out), "--session-id")
}

// defaultMaxJobs bounds how many background children one server will run at
// once. A model that loops on background=true should fill the cap and be told
// so, not fork until the machine does.
const defaultMaxJobs = 4

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
	// SessionID is the UUID passed as --session-id, once the binary accepts
	// it (WI-1126). The caller resumes the child by it. Empty until then.
	SessionID string `json:"session_id,omitempty"`
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
	// Bin is the codex executable. Empty means "codex" on PATH. The operator
	// points this at a binary that honours -s and -a: on a Vogt pod `codex`
	// is the full-access wrapper, which forces
	// --dangerously-bypass-approvals-and-sandbox and would undo the posture
	// this runner sets.
	Bin string
	// Sandbox is the child's -s value. Empty means workspace-write, the
	// narrowest posture that can still edit the checkout. danger-full-access
	// is only reachable through the server's operator flag, never a tool
	// argument, so a model cannot widen its own child.
	Sandbox string
	// Roots are the directories working_dir must fall inside, resolved
	// through symlinks. Empty means the server's own working directory.
	Roots []string
	// MaxDepth is how many times a child may itself spawn. 0 refuses at
	// depth 1, which is the default: one level, no grandchildren.
	MaxDepth int
	// MaxJobs caps concurrent background jobs. 0 means defaultMaxJobs.
	MaxJobs int
	// Depth is this server's own depth, read from DepthEnv. Tests set it
	// directly rather than through the environment.
	Depth int
	// Now names jobs. Tests pin it.
	Now func() time.Time

	mu   sync.Mutex
	jobs map[string]*job
	live int
	seq  int
}

type job struct {
	id     string
	done   chan struct{}
	result Result
	err    string
	// read is set once a poll has returned the finished result, which is
	// when prune may drop it.
	read bool
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
	if r.Depth > r.MaxDepth {
		return Result{}, fmt.Errorf("codex sub-agent depth %d exceeds the limit of %d", r.Depth, r.MaxDepth)
	}
	dir, err := r.confine(req.WorkingDir)
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
		return r.start(ctx, req, dir)
	}
	res, err := r.execute(ctx, req, dir)
	if err != nil {
		return Result{}, err
	}
	res.State = stateDone
	return res, nil
}

// confine resolves dir and checks it sits inside one of the roots.
//
// Symlinks are resolved on both sides before the prefix check. A root of
// /srv/work and a working_dir of /srv/work/link, where link points at
// /etc, must be refused: the child would otherwise get workspace-write
// wherever the link leads.
func (r *Runner) confine(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("working_dir is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("working_dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("working_dir: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("working_dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working_dir %s is not a directory", resolved)
	}
	roots := r.Roots
	if len(roots) == 0 {
		cwd, cerr := os.Getwd()
		if cerr != nil {
			return "", fmt.Errorf("server working directory: %w", cerr)
		}
		roots = []string{cwd}
	}
	for _, root := range roots {
		rr, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			continue
		}
		if resolved == rr || strings.HasPrefix(resolved, rr+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("working_dir %s is outside the server's roots", resolved)
}

func (r *Runner) start(ctx context.Context, req Request, dir string) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	cap := r.MaxJobs
	if cap <= 0 {
		cap = defaultMaxJobs
	}
	if r.live >= cap {
		return Result{}, fmt.Errorf("%d background jobs already running (limit %d)", r.live, cap)
	}
	r.seq++
	r.live++
	id := fmt.Sprintf("codexsub-%s-%d", r.now().UTC().Format("20060102-150405"), r.seq)
	j := &job{id: id, done: make(chan struct{})}
	if r.jobs == nil {
		r.jobs = map[string]*job{}
	}
	r.jobs[id] = j

	// The job outlives the tool call that started it, so it keeps its own
	// context rather than the request's, which the server cancels on return.
	// The runner's own context still bounds it: Close cancels that, which is
	// how a child dies when the server exits.
	go func() {
		defer close(j.done)
		res, err := r.execute(context.WithoutCancel(ctx), req, dir)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.live--
		if err != nil {
			j.err = err.Error()
			j.result = Result{Job: id, State: stateFailed, Error: j.err}
			return
		}
		res.State = stateDone
		res.Job = id
		j.result = res
	}()
	return Result{Job: id, State: stateRunning}, nil
}

// prune drops jobs that have finished and been read. A job nobody has polled
// yet stays, so a caller that is slow to check still finds it. The cap
// counts running jobs, not retained ones, so this only bounds memory.
func (r *Runner) prune() {
	for id, j := range r.jobs {
		if j.read {
			delete(r.jobs, id)
		}
	}
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
	j.read = true
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

	final, sessionID, err := r.codex(ctx, work, req)
	if err != nil {
		return Result{}, err
	}
	if verr := validate(req.OutputSchema, final); verr != nil {
		return Result{}, fmt.Errorf("final message does not match output_schema: %w", verr)
	}

	res := Result{Final: final, SessionID: sessionID}
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
// The posture is always explicit. -s workspace-write confines the child's
// writes to the checkout --cd names. There is no approval flag: `codex exec`
// has none (it is TUI-only, codex-rs/tui/src/cli.rs) and hard-codes approval
// Never (exec/src/lib.rs), so passing -a makes every spawn fail with a clap
// error.
//
// -s does not conflict with the full-access wrapper's
// --dangerously-bypass-approvals-and-sandbox. The conflicts_with on that
// flag belongs to --approve-for-me, so a wrapper that prepends it and then
// our -s parses and runs, and the child is unsandboxed. The server refuses
// such a binary at startup instead (see refuseWrapper), and --codex must
// name the real binary.
//
// --json prints an event per line; the last {"type":"item.completed"} whose
// item is an agent message is the answer. -o writes the same answer to a file,
// which is the fallback when the event stream has no such item. --ephemeral
// keeps the child out of the user's session list: it was spawned, not
// started by them.
func (r *Runner) codex(ctx context.Context, dir string, req Request) (string, string, error) {
	bin := r.bin()
	tmp, err := os.MkdirTemp("", "codexsub-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	last := filepath.Join(tmp, "last.txt")

	sandbox := r.Sandbox
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	args := []string{
		"exec", "--cd", dir,
		"-s", sandbox,
		"--json", "--ephemeral", "-o", last,
	}
	if len(req.OutputSchema) > 0 {
		schema := filepath.Join(tmp, "schema.json")
		if werr := os.WriteFile(schema, req.OutputSchema, 0o600); werr != nil {
			return "", "", werr
		}
		args = append(args, "--output-schema", schema)
	}
	// The session id has to be a UUID. WI-1126 parses --session-id as a
	// ThreadId, and the worktree's directory name is not one, so passing
	// that would make every spawn fail the moment the flag is accepted.
	sessionID := uuid.NewString()
	// TODO(WI-1126): the flag is withheld until the binary accepts it. See sessionIDSupported.
	if sessionIDSupported(bin) {
		args = append(args, "--session-id", sessionID)
	} else {
		sessionID = ""
	}
	args = append(args, req.Task)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// Depth travels in the environment because the child runs its own copy
	// of this server, and that copy has no other way to know it is a child.
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", DepthEnv, r.Depth+1))
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
		return "", "", fmt.Errorf("codex exec: %s", msg)
	}
	return final, sessionID, nil
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
