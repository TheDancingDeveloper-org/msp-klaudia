// Package hooks runs user-configured shell commands at fixed points in the
// agent's lifecycle, modeled on Claude Code's hooks. A hook is an external
// command that receives a JSON event payload on stdin and can, by its exit
// code and stdout, block the action that triggered it or feed extra context
// back to the model.
//
// Four events are supported: PreToolUse and PostToolUse (which match on the
// tool name), UserPromptSubmit and Stop.
//
// # I/O protocol (matches Claude Code)
//
//   - stdin: a JSON object with at least "hook_event_name", plus event-specific
//     fields (tool_name/tool_input for PreToolUse; those plus tool_response for
//     PostToolUse; prompt for UserPromptSubmit; nothing extra for Stop).
//   - exit 0: success. stdout MAY be a JSON object with fields like
//     {"decision":"block","reason":"…"} (block the default action) or
//     {"additionalContext":"…"} / {"hookSpecificOutput":{"additionalContext":"…"}}.
//     For UserPromptSubmit, non-JSON stdout is taken as additional context.
//   - exit 2: a blocking error. stderr is the reason fed back to the model.
//   - any other non-zero exit: a non-blocking error — logged and ignored.
//
// # Trust
//
// Hooks execute arbitrary commands, so only user-level (~/.klaudia) hooks are
// wired up by the CLI; project-level (.klaudia/config.toml) hooks are dropped
// at load time (see internal/config.Load). This package itself runs whatever
// configuration it is handed.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// EventName identifies a lifecycle point at which hooks may run.
type EventName string

// The supported events.
const (
	PreToolUse       EventName = "PreToolUse"
	PostToolUse      EventName = "PostToolUse"
	UserPromptSubmit EventName = "UserPromptSubmit"
	Stop             EventName = "Stop"
)

// DefaultTimeout is the per-hook wall-clock budget when a hook sets no timeout.
const DefaultTimeout = 60 * time.Second

// Hook is one command entry within a matcher group.
type Hook struct {
	// Type is the hook kind. Only "command" is supported; an empty value is
	// treated as "command".
	Type string `toml:"type,omitempty" json:"type,omitempty"`
	// Command is the shell command to run (via `sh -c`). The event payload is
	// written to its stdin as JSON.
	Command string `toml:"command,omitempty" json:"command,omitempty"`
	// Timeout is the per-hook budget in seconds. 0 uses DefaultTimeout.
	Timeout int `toml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Group is a matcher plus the hooks that fire when it matches. For PreToolUse
// and PostToolUse, Matcher is tested against the tool name (empty or "*" match
// every tool; otherwise it is an anchored regexp, falling back to exact string
// equality if it does not compile). For UserPromptSubmit and Stop the matcher
// is ignored and every group fires.
type Group struct {
	Matcher string `toml:"matcher,omitempty" json:"matcher,omitempty"`
	Hooks   []Hook `toml:"hooks,omitempty" json:"hooks,omitempty"`
}

// Config is the hooks section of the Klaudia config: each event name maps to a
// list of matcher groups. TOML shape (ergonomic, per the issue):
//
//	[[hooks.PreToolUse]]
//	matcher = "Bash"
//	[[hooks.PreToolUse.hooks]]
//	type = "command"
//	command = "…"
//	timeout = 30
type Config struct {
	PreToolUse       []Group `toml:"PreToolUse,omitempty" json:"PreToolUse,omitempty"`
	PostToolUse      []Group `toml:"PostToolUse,omitempty" json:"PostToolUse,omitempty"`
	UserPromptSubmit []Group `toml:"UserPromptSubmit,omitempty" json:"UserPromptSubmit,omitempty"`
	Stop             []Group `toml:"Stop,omitempty" json:"Stop,omitempty"`
}

// IsEmpty reports whether no hooks are configured for any event.
func (c Config) IsEmpty() bool {
	return len(c.PreToolUse)+len(c.PostToolUse)+len(c.UserPromptSubmit)+len(c.Stop) == 0
}

func (c Config) groups(event EventName) []Group {
	switch event {
	case PreToolUse:
		return c.PreToolUse
	case PostToolUse:
		return c.PostToolUse
	case UserPromptSubmit:
		return c.UserPromptSubmit
	case Stop:
		return c.Stop
	}
	return nil
}

// Input is the JSON payload written to a hook's stdin.
type Input struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse  json.RawMessage `json:"tool_response,omitempty"`
	Prompt        string          `json:"prompt,omitempty"`
	CWD           string          `json:"cwd,omitempty"`
}

// rawOutput is the JSON a hook may write to stdout on a successful (exit 0) run.
type rawOutput struct {
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	AdditionalContext string `json:"additionalContext"`
	// HookSpecificOutput mirrors Claude Code's nested form for additional
	// context (e.g. UserPromptSubmit).
	HookSpecificOutput *struct {
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// Decision is the aggregated outcome of running every matching hook for an
// event. Block is set if any hook blocked; Reason concatenates the blocking
// reasons; AdditionalContext concatenates any context the hooks emitted.
type Decision struct {
	Block             bool
	Reason            string
	AdditionalContext string
}

// Runner executes the hooks for a Config. A nil *Runner is valid and is a
// no-op — the CLI hands the agent nil when no hooks are configured.
type Runner struct {
	cfg Config
	cwd string
	// Logf, if set, receives non-blocking hook failures (a hook that could not
	// start, timed out, or exited non-zero with a code other than 2). Nil is
	// silent.
	Logf func(format string, args ...any)
}

// New returns a Runner for cfg, or nil if no hooks are configured (so the
// caller can pass the result straight through as the agent's nil-disables
// Hooks option). cwd is the working directory hooks run in.
func New(cfg Config, cwd string) *Runner {
	if cfg.IsEmpty() {
		return nil
	}
	return &Runner{cfg: cfg, cwd: cwd}
}

// Run executes every hook matching event with the given payload and returns the
// aggregated decision. A nil Runner returns a zero Decision. The returned error
// is only ever non-nil for a programming fault (payload that cannot marshal);
// hook execution failures are surfaced through Logf, not the error.
func (r *Runner) Run(ctx context.Context, event EventName, in Input) (Decision, error) {
	var dec Decision
	if r == nil {
		return dec, nil
	}
	in.HookEventName = string(event)
	if in.CWD == "" {
		in.CWD = r.cwd
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return dec, err
	}

	matchTool := event == PreToolUse || event == PostToolUse
	var reasons, contexts []string
	for _, g := range r.cfg.groups(event) {
		if matchTool && !matchMatcher(g.Matcher, in.ToolName) {
			continue
		}
		for _, h := range g.Hooks {
			hd, herr := r.runOne(ctx, h, payload, event)
			if herr != nil {
				if r.Logf != nil {
					r.Logf("hook %q (%s): %v", h.Command, event, herr)
				}
				continue
			}
			if hd.Block {
				dec.Block = true
				if hd.Reason != "" {
					reasons = append(reasons, hd.Reason)
				}
			}
			if hd.AdditionalContext != "" {
				contexts = append(contexts, hd.AdditionalContext)
			}
		}
	}
	dec.Reason = strings.Join(reasons, "\n")
	dec.AdditionalContext = strings.Join(contexts, "\n")
	return dec, nil
}

// runOne runs a single hook command. It returns a Decision for exit codes 0
// (parsed stdout) and 2 (blocking, stderr as reason); every other outcome —
// failure to start, timeout, or a non-zero exit other than 2 — is returned as a
// non-blocking error for the caller to log and ignore.
func (r *Runner) runOne(ctx context.Context, h Hook, payload []byte, event EventName) (Decision, error) {
	if strings.TrimSpace(h.Command) == "" {
		return Decision{}, nil
	}
	if h.Type != "" && h.Type != "command" {
		return Decision{}, fmt.Errorf("unsupported hook type %q", h.Type)
	}

	timeout := DefaultTimeout
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", h.Command)
	cmd.Stdin = bytes.NewReader(payload)
	if r.cwd != "" {
		cmd.Dir = r.cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the hook in its own process group and, on timeout, kill the whole
	// group. `sh -c "sleep …"` can leave a child holding the stdout/stderr pipes
	// after the shell is killed, and cmd.Run() blocks until those pipes close —
	// so killing only the shell would let the deadline be ignored until the
	// grandchild finished. WaitDelay is a backstop: if a descendant still holds
	// the pipes open, os/exec closes them and returns rather than hanging.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return Decision{}, fmt.Errorf("timed out after %s", timeout)
	}

	code := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		} else {
			return Decision{}, runErr // could not start the command
		}
	}

	switch code {
	case 0:
		return parseSuccess(event, stdout.Bytes()), nil
	case 2:
		// Blocking error: stderr is the reason, falling back to stdout.
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = strings.TrimSpace(stdout.String())
		}
		return Decision{Block: true, Reason: reason}, nil
	default:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return Decision{}, fmt.Errorf("exited %d: %s", code, msg)
	}
}

// parseSuccess interprets a successful hook's stdout.
func parseSuccess(event EventName, stdout []byte) Decision {
	var d Decision
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return d
	}
	if trimmed[0] == '{' {
		var out rawOutput
		if err := json.Unmarshal(trimmed, &out); err == nil {
			if strings.EqualFold(out.Decision, "block") {
				d.Block = true
				d.Reason = out.Reason
			}
			ac := out.AdditionalContext
			if ac == "" && out.HookSpecificOutput != nil {
				ac = out.HookSpecificOutput.AdditionalContext
			}
			d.AdditionalContext = ac
			return d
		}
		// Fell through: it looked like JSON but did not parse. Treat as plain.
	}
	// Non-JSON stdout is additional context for UserPromptSubmit (Claude Code's
	// convenience), and ignored for the other events.
	if event == UserPromptSubmit {
		d.AdditionalContext = string(trimmed)
	}
	return d
}

// matchMatcher reports whether a group's matcher applies to a tool name.
func matchMatcher(matcher, tool string) bool {
	matcher = strings.TrimSpace(matcher)
	if matcher == "" || matcher == "*" {
		return true
	}
	if re, err := regexp.Compile("^(?:" + matcher + ")$"); err == nil {
		return re.MatchString(tool)
	}
	return matcher == tool
}
