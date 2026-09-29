package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/greenthread-ai/klaudia/internal/native/bashparser"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/sandbox"
	"github.com/greenthread-ai/klaudia/internal/schema"
	"github.com/greenthread-ai/klaudia/internal/trust"
)

// bashDefaultTimeout is applied when the model doesn't specify one.
const bashDefaultTimeout = 2 * time.Minute

// BashInput is the Bash tool's input.
type BashInput struct {
	Command         string `json:"command" jsonschema:"description=The shell command to execute"`
	Description     string `json:"description,omitempty" jsonschema:"description=A short description of what the command does"`
	Timeout         int    `json:"timeout,omitempty" jsonschema:"description=Timeout in milliseconds (default 120000)"`
	RunInBackground bool   `json:"run_in_background,omitempty" jsonschema:"description=Run detached and return a shell id immediately; read its output with BashOutput and stop it with KillShell"`
}

// Bash executes shell commands via a sandbox.Executor. When run_in_background is
// set, it launches a managed job tracked by the (optional) JobStore.
type Bash struct {
	schema   *schema.Schema
	executor sandbox.Executor
	shells   *JobStore
}

// NewBash constructs the Bash tool with the given executor. The optional
// JobStore backs run_in_background (omit it to disable background jobs).
func NewBash(executor sandbox.Executor, shells ...*JobStore) (*Bash, error) {
	s, err := schema.For[BashInput]()
	if err != nil {
		return nil, fmt.Errorf("bash: build schema: %w", err)
	}
	b := &Bash{schema: s, executor: executor}
	if len(shells) > 0 {
		b.shells = shells[0]
	}
	return b, nil
}

func (b *Bash) Name() string { return "Bash" }

func (b *Bash) Description(context.Context) (string, error) {
	return "Executes a shell command and returns its combined output. Commands run via bash. " +
		"Provide an optional timeout in milliseconds (default 120000, max 600000). " +
		"Prefer the Read/Glob/Grep tools over cat/find/grep where possible. " +
		// Observed in a real session: the model appended `; echo \"EXIT_STATUS: $?\"`
		// to capture a status that was already being reported, and in doing so
		// made the shell exit 0 every time — so the failure never surfaced as
		// one. Say plainly that the status is already there.
		"A non-zero exit is reported for you as \"[exit code N]\" and marks the call failed; " +
		"do not append `; echo $?` or similar, which makes the shell exit 0 and hides the failure.", nil
}

func (b *Bash) InputSchema() json.RawMessage { return b.schema.Raw }

func (b *Bash) ValidateInput(raw json.RawMessage) error {
	if err := b.schema.Validate(raw); err != nil {
		return err
	}
	var in BashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Command) == "" {
		return fmt.Errorf("command must not be empty")
	}
	return nil
}

// PermissionRequest derives a rule specifier from the command via the bash
// parser (e.g. "git status"), falling back to the raw command on parse error.
//
// RuleSpecifiers carries the per-command specifiers to persist on "always
// allow" (see bashRuleSpecifiers) so a compound line saves a rule for every
// command it runs, not only the first. Commands carries the per-command rule
// forms the rules are actually checked against (see bashRuleCommands), so every
// command in the line is matched, not only the first.
func (b *Bash) PermissionRequest(raw json.RawMessage) permission.PermissionRequest {
	var in BashInput
	_ = json.Unmarshal(raw, &in)
	req := permission.PermissionRequest{Specifier: in.Command}
	if a, err := bashparser.Parse(in.Command); err == nil {
		if p := a.Prefix(); p != "" {
			req.Specifier = p
		}
	}
	req.RuleSpecifiers = bashRuleSpecifiers(in.Command)
	req.Commands, req.Opaque = bashRuleCommands(in.Command, 0)
	return req
}

// bashRuleSpecifiers returns the allow-rule specifiers to persist when the user
// chooses "always allow" for command.
//
// A single simple command yields exactly one specifier equal to Prefix() — the
// same string saved before this change — so a lone command's saved rule is
// unchanged. A compound line (chained or piped) yields one "program subcommand"
// short form per distinct command, so that under every-command rule matching
// (PR #16) each command in the line is individually allowed rather than only
// the first. Saving a single first-command rule either over-permitted (the old
// first-command-only matching) or, once rules must match every command, would
// silently fail to allow the same line next time.
//
// It returns nil — the caller then falls back to the single Specifier, the
// prior behaviour — whenever the line cannot be reduced to a clean set of named
// commands: a parse error, any parameter expansion or command substitution
// (its text is not the whole command, so a rule built from it could both
// over-permit and miss the substituted command), or an inline shell / eval
// whose payload commands are not enumerated here. In those cases guessing a
// rule set risks over-permitting and would not reliably allow the line anyway,
// so the conservative single-rule fallback is preferred.
func bashRuleSpecifiers(command string) []string {
	a, err := bashparser.Parse(command)
	if err != nil || len(a.Commands) == 0 {
		return nil
	}
	// An expansion anywhere means at least one word is not the whole story;
	// refuse the line rather than name a command from partial text.
	if a.HasExpansion {
		return nil
	}
	var specs []string
	seen := make(map[string]bool)
	for _, c := range a.Commands {
		// A program that is an expansion, or an inline shell whose script this
		// pass does not read, cannot be named by a clean rule.
		if c.Name == "" || !c.NameWord.Literal || isInlineShellCommand(c.Name) {
			return nil
		}
		spec := commandShortForm(c.Name, c.Args)
		if spec == "" {
			return nil
		}
		if !seen[spec] {
			seen[spec] = true
			specs = append(specs, spec)
		}
	}
	return specs
}

// commandShortForm is a command's "program subcommand" specifier: the program
// name plus its first non-flag argument ("git status"), or the name alone. It
// matches the form bashparser.Prefix gives the first command, applied per
// command.
func commandShortForm(name string, args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return name + " " + arg
		}
	}
	return name
}

// isInlineShellCommand reports whether name is a shell or eval that runs a
// script passed as an argument — bash -c '…', eval '…' — whose inner commands
// this pass does not enumerate, so the line is not cleanly reducible to rules.
func isInlineShellCommand(name string) bool {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh", "eval":
		return true
	}
	return false
}

const (
	// maxRuleCommandLen is the longest command line allow rules may approve.
	// Past it, a line is asked about whatever the rules say: a prompt cannot
	// usefully show it, and length is how a tail gets hidden.
	maxRuleCommandLen = 10000
	// maxPayloadDepth bounds the recursion into `bash -c` and eval scripts.
	maxPayloadDepth = 3
)

// bashRuleCommands lists the commands a Bash line runs, each with the forms a
// permission rule may name it by (see permission.PermissionRequest.Commands).
// opaque is true when some command could not be read.
func bashRuleCommands(line string, depth int) (cmds [][]string, opaque bool) {
	if len(line) > maxRuleCommandLen || depth > maxPayloadDepth {
		return nil, true
	}
	a, err := bashparser.Parse(line)
	if err != nil {
		return nil, true
	}
	for _, c := range a.Commands {
		if !c.NameWord.Literal {
			opaque = true // `$CMD rm -rf x` names no program a rule can match
			continue
		}
		forms := commandForms(c.Name, c.Args, nil)
		name, args, ok := trust.Unwrapped(c)
		if !ok {
			opaque = true // a wrapper around an expansion: `sudo "$X" …`
			cmds = append(cmds, forms)
			continue
		}
		cmds = append(cmds, commandForms(name, args, forms))
		// The script of `bash -c` or eval — found after unwrapping, so
		// `sudo bash -c '…'` is read too — runs commands of its own, each
		// checked like the rest. One that is an expansion cannot be read.
		if payload, isShell := bashparser.ShellPayload(name, args); isShell {
			if hasExpansion(c.ArgWords) {
				opaque = true
				continue
			}
			inner, innerOpaque := bashRuleCommands(payload, depth+1)
			cmds = append(cmds, inner...)
			opaque = opaque || innerOpaque
		}
	}
	return cmds, opaque
}

// hasExpansion reports whether any word is an expansion rather than literal.
func hasExpansion(words []bashparser.Word) bool {
	for _, w := range words {
		if !w.Literal {
			return true
		}
	}
	return false
}

// commandForms appends to forms, without duplicates, the ways a rule may name
// a command: in full and in short form, by the program as written and by its
// base name, so `Bash(rm:*)` also covers `/bin/rm -rf x`.
func commandForms(name string, args []string, forms []string) []string {
	add := func(f string) {
		for _, have := range forms {
			if have == f {
				return
			}
		}
		forms = append(forms, f)
	}
	for _, n := range []string{name, bashparser.Base(name)} {
		add(strings.TrimSpace(n + " " + strings.Join(args, " ")))
		add(bashparser.ShortForm(n, args))
	}
	return forms
}

// CheckPermissions: Bash is a command-executing (exec-class) tool.
func (b *Bash) CheckPermissions(pctx permission.Context, _ permission.PermissionRequest) permission.Decision {
	return execClassDecision(pctx)
}

func (b *Bash) Execute(ctx context.Context, tctx Context, raw json.RawMessage) ([]Result, error) {
	var in BashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}

	// Refuse a command that needs a terminal before launching it. Without this
	// the child sits waiting for a keypress that cannot arrive and the turn
	// stalls until the timeout, reporting nothing useful about why.
	if reason, blocked := sandbox.TTYRequired(in.Command); blocked {
		return []Result{{Content: reason, IsError: true}}, nil
	}

	// A service the model backgrounds with `&` itself skips the whole job
	// system: no name, no managed log, no crash detection, no restart. Observed
	// in the agent-loop torture test — the model shell-backgrounded a dev
	// server eleven times and then managed the processes by hand with pkill and
	// `kill -9 $(lsof -ti:PORT)`, which is exactly the work managed jobs exist
	// to remove.
	//
	// The timeout nudge cannot catch this: a self-backgrounded command returns
	// immediately, so there is no timeout. It has to be caught on the way in.
	if !in.RunInBackground {
		if reason, blocked := selfBackgrounded(in.Command); blocked {
			return []Result{{Content: reason, IsError: true}}, nil
		}
	}

	// Background: launch detached, return a shell id immediately.
	if in.RunInBackground {
		if b.shells == nil {
			return []Result{{Content: "background execution is not available", IsError: true}}, nil
		}
		res, err := b.shells.Start(b.executor, sandbox.Request{Command: in.Command, WorkingDir: tctx.WorkingDir})
		if err != nil {
			return []Result{{Content: fmt.Sprintf("Failed to start background command: %v", err), IsError: true}}, nil
		}
		j := res.Job
		if res.Duplicate {
			// Two dev servers fighting over one port is a confusing failure, and
			// the loser usually looks like broken code. Hand back the one that
			// is already up instead.
			return []Result{{Content: fmt.Sprintf(
				"That command is already running as job %s (%s), started %s ago. "+
					"Reusing it rather than starting a second copy — read its output with "+
					"BashOutput(bash_id=%q), or restart it with RestartJob(job=%q).",
				j.Name, j.ID, fmtShortDuration(time.Since(j.Started)), j.Name, j.Name)}}, nil
		}
		return []Result{{Content: fmt.Sprintf(
			"Started job %s (%s). Read its output with BashOutput(bash_id=%q), restart it with "+
				"RestartJob(job=%q), stop it with KillShell(shell_id=%q).",
			j.Name, j.ID, j.Name, j.Name, j.Name)}}, nil
	}

	timeout := bashDefaultTimeout
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Millisecond, 10*time.Minute)
	}

	resp, err := b.executor.Run(ctx, sandbox.Request{
		Command:    in.Command,
		WorkingDir: tctx.WorkingDir,
		Timeout:    timeout,
	})
	if err != nil {
		return []Result{{Content: fmt.Sprintf("Failed to run command: %v", err), IsError: true}}, nil
	}

	model, full := formatBashOutput(resp, in.Command)
	failed := resp.ExitCode != 0 && !noMatchExit(in.Command, resp)
	return []Result{{Content: model, Full: full, IsError: failed}}, nil
}

// selfBackgrounded reports whether a command detaches a long-running service
// with the shell rather than asking for a job, and what to do instead.
//
// Deliberately narrow: it fires only when a command that is *itself*
// backgrounded looks like a service. `sleep 1 &` in a test script is nobody's
// business; `go run ./cmd/api &` is a dev server that should have a name, a log
// and a way to be restarted.
//
// The line is parsed rather than scanned, which is what keeps it narrow: `&>`
// and `&>>` are redirections, not backgrounding; `go test ./server/... &` is
// not `serve`; and `a & b & wait` detaches nothing, because the shell does not
// return until both finish.
func selfBackgrounded(command string) (reason string, blocked bool) {
	if !detachesLongRunning(command, 0) {
		return "", false
	}
	return "This backgrounds a long-running process with the shell, which leaves it untracked: " +
		"no name, no log Klaudia can page, no notice when it dies, and no way to restart it — " +
		"you would have to hunt it down with ps and kill.\n\n" +
		"Start it as a managed job instead: run the server on its own with Bash's " +
		"run_in_background parameter, then run your checks as a separate command. " +
		"Read its output with BashOutput, restart it with RestartJob, stop it with KillShell, " +
		"and check Jobs first in case it is already up.\n\n" +
		"If this really is short-lived, just run it in the foreground.", true
}

// detachesLongRunning is selfBackgrounded's test. depth bounds the descent
// into `bash -c '…'` payloads.
//
// A line that does not parse is let through: bash will refuse it on its own,
// and guessing at its structure is how `&>` came to be read as `&`.
func detachesLongRunning(command string, depth int) bool {
	if depth > 3 {
		return false
	}
	a, err := bashparser.Parse(command)
	if err != nil {
		return false
	}
	for i, c := range a.Commands {
		payloads := bashparser.Analysis{Commands: []bashparser.Command{c}}.ShellPayloads()
		for _, p := range payloads {
			if detachesLongRunning(p, depth+1) {
				return true
			}
		}

		words, wrapper := unwrapDetacher(commandWords(c))
		if !c.Background && wrapper == "" {
			continue
		}
		long := wordsLongRunning(words, a.HasPipe)
		for _, p := range payloads {
			long = long || looksLongRunning(p)
		}
		if !long {
			continue
		}
		// `a & b & wait` returns only when a and b have: nothing outlives the
		// command. setsid is exempt from the exemption — it can fork a child
		// that wait never sees.
		if c.Background && wrapper != "setsid" && waitsLater(a.Commands[i+1:]) {
			continue
		}
		return true
	}
	return false
}

// unwrapDetacher strips nohup / setsid (and setsid's flags) from the front of
// a command, returning what they run and the last wrapper seen. Either one
// detaches by intent even without a trailing `&`.
func unwrapDetacher(words []string) (rest []string, wrapper string) {
	for len(words) > 0 && (words[0] == "nohup" || words[0] == "setsid") {
		wrapper = words[0]
		words = words[1:]
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			words = words[1:]
		}
	}
	return words, wrapper
}

// waitsLater reports whether a `wait` follows, which holds the shell until its
// background children finish.
func waitsLater(rest []bashparser.Command) bool {
	for _, c := range rest {
		if filepath.Base(c.Name) == "wait" && !c.Background {
			return true
		}
	}
	return false
}

// commandWords is a command as lowercase words, program name first and
// stripped of its directory (so ./node_modules/.bin/vite is vite).
func commandWords(c bashparser.Command) []string {
	if c.Name == "" {
		return nil
	}
	words := make([]string, 0, len(c.Args)+1)
	words = append(words, strings.ToLower(filepath.Base(c.Name)))
	for _, a := range c.Args {
		words = append(words, strings.ToLower(a))
	}
	return words
}

// runHints are the shapes that are only ever backgrounded because they do not
// return. Broader than serviceHints on purpose, and used only by
// selfBackgrounded: there a false positive costs one retry, and the guidance
// says exactly what to do — so erring toward catching `go run ./cmd/api &` is
// worth the occasional `go run ./cmd/oneshot &` being told to pick a lane.
var runHints = hintWords(
	"go run", "cargo run", "dotnet run", "bootrun",
	"rails s", "flask", "php -s", "caddy run", "nginx",
)

// serviceHints are the shapes of a command that does not intend to finish,
// each a run of whole words: `serve` matches `npx serve` and not `server`,
// `vite` matches `npx vite` and not `vitest`, `air` is the program and not a
// fragment of `repair`.
var serviceHints = hintWords(
	"run dev", "run start", "run serve", "run watch", "start:dev",
	"npm start", "yarn start", "pnpm start", "bun run dev",
	"compose up", "docker run", "serve", "http.server", "runserver",
	"tail -f", "watch", "nodemon", "vite", "webpack-dev-server",
	"rails s", "flask run", "uvicorn", "gunicorn", "air", "reflex",
	"ng serve", "next dev", "nuxt dev", "remix dev", "astro dev",
	"make dev", "make run", "make serve", "cargo watch", "mvn spring-boot:run",
)

func hintWords(hints ...string) [][]string {
	out := make([][]string, len(hints))
	for i, h := range hints {
		out[i] = strings.Fields(h)
	}
	return out
}

// looksLongRunning is looksLikeService plus runHints, for a whole line.
func looksLongRunning(command string) bool {
	a, err := bashparser.Parse(command)
	if err != nil {
		return false
	}
	for _, c := range a.Commands {
		if wordsLongRunning(commandWords(c), a.HasPipe) {
			return true
		}
	}
	return false
}

// wordsLongRunning matches one command's words against runHints, and against
// serviceHints unless the line is a pipeline.
func wordsLongRunning(words []string, piped bool) bool {
	return matchesHint(words, runHints) || (!piped && matchesHint(words, serviceHints))
}

// looksLikeService reports whether any command in the line matches a service
// hint. Only used to add a suggestion to a timeout, so a false positive costs
// one unhelpful sentence rather than a wrong decision.
func looksLikeService(command string) bool {
	a, err := bashparser.Parse(command)
	// A pipeline that ends somewhere is not a service, even if it starts with
	// one: `tail -f log | head -20` terminates.
	if err != nil || a.HasPipe {
		return false
	}
	for _, c := range a.Commands {
		if matchesHint(commandWords(c), serviceHints) {
			return true
		}
	}
	return false
}

// matchesHint reports whether any hint appears in words as a contiguous run of
// whole words.
func matchesHint(words []string, hints [][]string) bool {
	for _, h := range hints {
		for i := 0; i+len(h) <= len(words); i++ {
			if slices.Equal(words[i:i+len(h)], h) {
				return true
			}
		}
	}
	return false
}

// formatBashOutput combines stdout/stderr and annotates non-zero exit / timeout.
// It returns two strings: the model-facing text, clamped to bashMaxOutput (see
// output.go for why it keeps a tail as well as a head), and the untruncated
// text for local display. full is empty when nothing was clamped.
func formatBashOutput(resp sandbox.Response, command string) (model, full string) {
	var b strings.Builder
	b.WriteString(resp.Stdout)
	if resp.Stderr != "" {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString(resp.Stderr)
	}
	raw := b.String()
	out := raw
	if clamped, elided := clampOutput(raw); elided > 0 {
		out = clamped
		// The spill file is the *model's* escape hatch to the elided middle —
		// it can Read or grep the path. The UI doesn't need it: `full` carries
		// the same text in memory.
		if path, ok := spillOutput(raw); ok {
			out += "\n" + spillMarker + path + "]"
		}
		full = raw
	}

	// Status annotations belong on both variants — a reader of the full output
	// still needs to know the command failed.
	var status string
	if resp.TimedOut {
		status = fmt.Sprintf("\n[command timed out, exit code %d]", resp.ExitCode)
		// A bare exit 124 reads as "the command is broken". Usually it means the
		// command has no natural end, and the right move is to make it a job —
		// which keeps it running, gives it a name and a log, and lets the turn
		// continue. Saying so here is the difference between the model
		// retrying with a longer timeout and it doing the right thing.
		if looksLikeService(command) {
			status += "\n[this looks like a long-running service. Start it with Bash run_in_background " +
				"to make it a managed job: it keeps running, gets a name and a log, and you can carry on. " +
				"Check Jobs first — it may already be up.]"
		}
	} else if resp.Canceled {
		// Say plainly what happened, because the alternative is the model
		// inferring a cause. Whatever the command had already done still
		// happened, so the next move is to check the state rather than assume
		// the work failed or re-run it blindly.
		status = "\n[interrupted by the user before it finished — this was not a timeout and not a " +
			"failure of the command. Anything it had already done still took effect; check the " +
			"current state before re-running it.]"
	} else if noMatchExit(command, resp) {
		status = "\n[exit code 1: no match / differences found — not an error]"
	} else if resp.ExitCode != 0 {
		status = fmt.Sprintf("\n[exit code %d]", resp.ExitCode)
	}
	out += status
	if full != "" {
		full += status
	}

	if out == "" {
		return "[no output]", ""
	}
	return out, full
}

// answerExit1 are programs whose exit status 1 is an answer, not a failure:
// grep and friends found no match, diff and cmp found a difference, test
// found its condition false. (2 and above still mean an error.)
var answerExit1 = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"diff": true, "cmp": true, "test": true, "[": true,
}

// noMatchExit reports whether a command's exit 1 came from such a program,
// judged by the last command in the line — the one whose status the shell
// returns (without pipefail).
//
// Every non-zero exit used to be an error result, and error results feed
// the loop's repeat-failure steering: a model grepping for something that is
// correctly absent was nudged as if it kept making the same mistake.
func noMatchExit(command string, resp sandbox.Response) bool {
	if resp.ExitCode != 1 || resp.TimedOut || resp.Canceled {
		return false
	}
	a, err := bashparser.Parse(command)
	if err != nil || len(a.Commands) == 0 {
		return false
	}
	last := a.Commands[len(a.Commands)-1]
	return answerExit1[bashparser.Base(last.Name)]
}
