package hooks

import (
	"context"
	"fmt"
	"sync"

	"github.com/greenthread-ai/klaudia/internal/config"
)

// Runner holds a session's hooks and the state of the one trust decision they
// need. A nil *Runner is inert, so a frontend that has not wired hooks up calls
// Run freely and gets an empty Result.
type Runner struct {
	user    []Hook
	project []Hook
	// fingerprint identifies the project hook set, so approving it approves
	// exactly what was shown and an edit re-asks. See trust.go.
	fingerprint string
	projectFile string

	cwd       string
	sessionID string

	// Store records an approval across sessions. Zero value writes to
	// ~/.klaudia/hooks.json; tests substitute their own.
	Store Store

	mu sync.Mutex
	// notices is drained by the next Run; see Load.
	notices []string
	// started records that SessionStart has fired. See ClaimSessionStart.
	started bool

	// askMu guards the project trust decision. Separate from mu because it is
	// held across the prompt, which waits on a human.
	askMu sync.Mutex
	// decided/allowed memoise the session's answer. Without this a repo with a
	// PreToolUse hook would prompt before every tool call.
	decided bool
	allowed bool
}

// Confirm asks the user whether the repository's hooks may run on this machine,
// showing them the commands and the file they came from.
//
// It is a parameter of Run rather than a field on Runner because the thing that
// can ask a question is owned by the frontend and, in the TUI, is rebuilt every
// turn: a Runner that captured one would be holding a stale route to the user. Nil
// means no — the right answer for a headless run, where nobody is there to
// agree and "nobody objected" is not agreement.
type Confirm func(ctx context.Context, hooks []Hook, file string) bool

// Load builds a Runner from ~/.klaudia/config.toml and ./.klaudia/config.toml.
//
// It returns nil when neither file declares a hook, which is the common case and
// the one that should cost nothing: no runner means the loop's hook calls are
// nil-receiver no-ops rather than four function calls per tool that walk empty
// slices.
func Load(cwd, sessionID string) *Runner {
	src := config.LoadHooks(cwd)
	if len(src.User) == 0 && len(src.Project) == 0 {
		return nil
	}
	userFile := "~/.klaudia/config.toml"
	user, userErrs := compile(src.User, userFile, false)
	project, projErrs := compile(src.Project, src.ProjectPath, true)

	r := &Runner{
		user:        user,
		project:     project,
		fingerprint: fingerprint(project),
		projectFile: src.ProjectPath,
		cwd:         cwd,
		sessionID:   sessionID,
	}
	// Config problems are queued rather than returned. They are discovered at
	// load time but only mean something to the user, and the loop already has a
	// channel for things the user should see — the notice event. Draining them
	// through the first Run keeps every caller from needing a second error path
	// for "your config has a typo in it".
	for _, err := range append(userErrs, projErrs...) {
		r.notices = append(r.notices, err.Error())
	}
	return r
}

// New builds a Runner from already-compiled entries. For tests and for callers
// that get their configuration from somewhere other than a config file.
func New(cwd, sessionID string, user, project []config.Hook) *Runner {
	u, uerr := compile(user, "user config", false)
	p, perr := compile(project, "project config", true)
	r := &Runner{
		user:        u,
		project:     p,
		fingerprint: fingerprint(p),
		projectFile: "project config",
		cwd:         cwd,
		sessionID:   sessionID,
	}
	for _, err := range append(uerr, perr...) {
		r.notices = append(r.notices, err.Error())
	}
	return r
}

// Has reports whether any hook is attached to ev. The loop uses it to skip the
// work of building an Input — marshalling a tool's arguments and collapsing its
// result — for an event nobody is listening to.
func (r *Runner) Has(ev Event) bool {
	if r == nil {
		return false
	}
	for _, h := range append(r.user, r.project...) {
		if h.Event == ev {
			return true
		}
	}
	return false
}

// All returns every configured hook, user hooks first. For /doctor.
func (r *Runner) All() []Hook {
	if r == nil {
		return nil
	}
	return append(append([]Hook{}, r.user...), r.project...)
}

// ProjectApproved reports whether this project's hook set may already run,
// without asking anybody.
//
// It reads the store rather than the session's memoised answer, so /doctor can
// tell a user that their repo's hooks are configured-but-dormant before the
// first tool call has had a chance to raise the prompt. That is the state worth
// reporting: a hook that is written down and never fires looks identical to a
// hook that does not work.
func (r *Runner) ProjectApproved() bool {
	if r == nil || len(r.project) == 0 {
		return false
	}
	r.askMu.Lock()
	defer r.askMu.Unlock()
	if r.decided {
		return r.allowed
	}
	return r.Store.approved(r.cwd, r.fingerprint)
}

// ClaimSessionStart reports whether SessionStart still needs to fire, and marks
// it as fired.
//
// The agent loop runs once per user turn, not once per session, so "the first
// Run" is not the same thing as "the session started" — in the TUI it happens
// again with every message. The claim lives here because the Runner is the only
// object with the session's lifetime: a Loop is per-process but a sub-agent
// builds its own, and keying off that would fire SessionStart again the first
// time the Agent tool was used.
func (r *Runner) ClaimSessionStart() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return false
	}
	r.started = true
	return true
}

// drainNotices takes the queued config problems. They are reported once.
func (r *Runner) drainNotices() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.notices
	r.notices = nil
	return n
}

// selected returns the hooks that apply to this event, in order: the user's
// first, then the project's if they are allowed to run.
//
// User hooks run unconditionally. The person who wrote ~/.klaudia/config.toml is
// the person Klaudia is working for, and asking them to confirm their own
// settings file is the kind of prompt that teaches people to stop reading
// prompts.
func (r *Runner) selected(ctx context.Context, in Input, confirm Confirm, res *Result) []Hook {
	var out []Hook
	for _, h := range r.user {
		if h.Event == in.Event && h.matches(in.ToolName) {
			out = append(out, h)
		}
	}
	var fromProject []Hook
	for _, h := range r.project {
		if h.Event == in.Event && h.matches(in.ToolName) {
			fromProject = append(fromProject, h)
		}
	}
	if len(fromProject) == 0 {
		return out
	}
	allowed, notice := r.projectAllowed(ctx, confirm)
	if notice != "" {
		res.Notices = append(res.Notices, notice)
	}
	if !allowed {
		return out
	}
	return append(out, fromProject...)
}

// projectAllowed resolves, once per session, whether the repository's hooks may
// run on this machine.
//
// askMu is held across the whole decision, including the prompt. Dispatch
// groups run concurrency-safe tools in parallel, so several PreToolUse
// evaluations arrive here at once; a lock taken only to record the answer lets
// all of them ask first, and the user gets one prompt per call in the batch for
// a question with a single answer. Blocking the others on the prompt is the
// point — they are waiting for a decision that is being made.
func (r *Runner) projectAllowed(ctx context.Context, confirm Confirm) (bool, string) {
	r.askMu.Lock()
	defer r.askMu.Unlock()
	if r.decided {
		return r.allowed, ""
	}
	allowed, notice := r.askProject(ctx, confirm)
	r.decided, r.allowed = true, allowed
	return allowed, notice
}

func (r *Runner) askProject(ctx context.Context, confirm Confirm) (bool, string) {
	if r.Store.approved(r.cwd, r.fingerprint) {
		return true, ""
	}
	if confirm == nil {
		return false, fmt.Sprintf(
			"%s declares %s, which will not run: hooks from a repository need confirming interactively, and nobody is here to confirm them.",
			r.projectFile, plural(len(r.project), "hook"))
	}
	if !confirm(ctx, r.project, r.projectFile) {
		return false, fmt.Sprintf("Project hooks from %s will not run this session.", r.projectFile)
	}
	if err := r.Store.approve(r.cwd, r.fingerprint); err != nil {
		// The hooks still run — the user just agreed to them. They will simply
		// be asked again next session, which is a smaller failure than
		// overriding a decision that was made out loud.
		return true, fmt.Sprintf("could not record the hook approval (%v); you will be asked again next session", err)
	}
	return true, ""
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
