// Package permission records the user's stance for a session and applies it to
// tool calls.
//
// Almost everything this package used to decide is now decided by zone, before
// it is consulted: agent.HostGate classifies each tool call and stops changes
// to the machine Klaudia is running on. What is left is the stance itself —
// get on with it, look but do not touch, or check nothing.
//
// There are no allow/deny rules here any more. Per-command approval was the
// model the zone model replaced, and keeping a second, overlapping gate meant
// two things that could each permit a call and neither of which described the
// other. The rules were also a trap in practice: one in a config demoted the
// next session to a mode that asked about everything, so the answer offered to
// stop a prompt was the thing that guaranteed more of them.
//
// It is a leaf package (no project imports) so both the tools and agent
// packages can depend on it without import cycles.
package permission

// Mode is the user's stance for the session.
//
// Four, not six. The old default and dontAsk asked the user to pick a stance on
// file edits versus commands versus network, which is a question about tool
// categories and never had a good answer. Zones answer it now, so what is left
// is genuinely different intents. acceptEdits came back (WI-1272) as one of
// them: "edit this project freely, but show me every command" is a posture a
// supervisor of a driven session wants, and nothing else expressed it.
type Mode string

const (
	// ModeAutonomous lets Klaudia finish the task without per-action prompts.
	//
	// The default, and the mode the trust model is built for. What used to be
	// a per-command question is answered by zone: project work runs, and
	// changes to this machine are stopped by the host gate before this package
	// is consulted.
	ModeAutonomous Mode = "autonomous"
	// ModeAcceptEdits auto-approves file edits inside the workspace (the
	// working directory and any added directories) and asks about everything
	// else that has an effect: commands, network and browser tools, MCP calls,
	// and edits outside the workspace. Claude Code's acceptEdits, without its
	// allow rules. The host gate runs ahead of it exactly as in autonomous.
	ModeAcceptEdits Mode = "acceptEdits"
	// ModeBypassPermissions allows everything, including the host gate
	// (--dangerously-skip-permissions).
	ModeBypassPermissions Mode = "bypassPermissions"
	// ModePlan is read-only exploration; mutations are blocked.
	ModePlan Mode = "plan"
)

// Valid reports whether m is a recognized mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeAutonomous, ModeAcceptEdits, ModeBypassPermissions, ModePlan:
		return true
	}
	return false
}

// Label is a human-friendly description of the mode for the UI (no raw
// identifiers).
func (m Mode) Label() string {
	switch m {
	case ModeAutonomous:
		return "Autonomous — finish the task; ask before changing this machine"
	case ModeAcceptEdits:
		return "Accept edits — edit files in the workspace freely; ask before commands, network and anything else"
	case ModeBypassPermissions:
		return "Bypass ALL permission checks (dangerous — tools run without asking)"
	case ModePlan:
		return "Plan mode — read-only, mutations blocked"
	default:
		return string(m)
	}
}

// SelectableModes are the modes a user may switch to interactively. acceptEdits
// joined after plan, so the picker numbers people already knew for autonomous
// and plan did not move; bypass stays last.
func SelectableModes() []Mode {
	return []Mode{ModeAutonomous, ModePlan, ModeAcceptEdits, ModeBypassPermissions}
}

// Behavior is the outcome of a permission check.
type Behavior string

const (
	Allow Behavior = "allow"
	Deny  Behavior = "deny"
	Ask   Behavior = "ask"
)

// Decision is the result of a permission evaluation. Message explains a deny/ask.
type Decision struct {
	Behavior Behavior
	Message  string
}

// Context carries the active mode for a run, and what the run counts as its
// workspace.
//
// Mode is a function rather than a value so each check reads the live session
// setting at decision time — when a user types `/mode bypass` mid-turn (or mid
// /goal-iteration), subsequent tool dispatches see the new mode without waiting
// for the next TUI turn boundary. Callers with a fixed mode (tests, headless
// single-shot) wrap their value with StaticMode.
type Context struct {
	Mode func() Mode
	// InWorkspace reports whether a path, as written in a tool's input, lies
	// in the run's workspace: its working directory or an added directory.
	// Only acceptEdits consults it. Nil means no path is known to be in the
	// workspace, so acceptEdits asks about every edit rather than guessing.
	// The agent loop fills it per run (it knows the working directory, which
	// differs for a sub-agent in its own worktree); see agent.permFor.
	InWorkspace func(path string) bool
}

// PathInWorkspace reports c.InWorkspace(path), false when it is unset.
func PathInWorkspace(c Context, path string) bool {
	return c.InWorkspace != nil && c.InWorkspace(path)
}

// StaticMode returns a Mode-function that always reports m. Convenience for
// callers without a live mode source — tests, headless one-shot runs, and any
// spot where the mode genuinely cannot change for the lifetime of the Context.
func StaticMode(m Mode) func() Mode {
	return func() Mode { return m }
}

// CurrentMode returns c.Mode() with a nil-safe default of ModeAutonomous.
//
// Autonomous is the right default for a zero-value Context because it is not
// the permissive end of the scale any more: the host gate runs ahead of this
// package and is unaffected by the mode. A Context with no mode source is a
// test or an embedding caller, and either way "do the work, stop at the host
// boundary" is the behaviour they mean.
func CurrentMode(c Context) Mode {
	if c.Mode == nil {
		return ModeAutonomous
	}
	return c.Mode()
}

// IntrinsicChecker is implemented by a tool to express its own permission
// stance given the mode (e.g. Read always allows; Edit is blocked in plan).
type IntrinsicChecker interface {
	// Name is the tool name, used in messages.
	Name() string
	// CheckPermissions returns the tool's intrinsic decision for this input,
	// considering the current mode.
	CheckPermissions(pctx Context, perm PermissionRequest) Decision
}

// PermissionRequest describes the concrete action a tool wants to take.
type PermissionRequest struct {
	// Specifier is a short description of the action (a file path for Edit,
	// the command prefix for Bash). Display only — it labelled rule matches
	// before the rules were removed, and now only reaches a frontend that is
	// showing the user what is being asked about.
	Specifier string
}

// Check evaluates the permission flow for a tool invocation: bypass allows
// everything, otherwise the tool decides for itself.
//
// The host gate has already run by this point (see agent.dispatch) — that
// ordering is deliberate and is where a change to this machine is stopped.
func Check(pctx Context, tool IntrinsicChecker, req PermissionRequest) Decision {
	if CurrentMode(pctx) == ModeBypassPermissions {
		return Decision{Behavior: Allow}
	}
	return tool.CheckPermissions(pctx, req)
}

// legacyModes are the retired mode names (default, dontAsk). Upstream removed
// them; this fork still accepts them, as deprecated aliases for ModeAutonomous,
// because launchers were built against them and an unknown mode exits 2 before
// the session starts. The alias only selects the mode: no allow/deny rules
// come back with it.
//
// acceptEdits was one of these until WI-1272 made it a real mode. A launcher
// that passed it to mean "autonomous" now gets the stricter posture — commands
// ask — which is what the name always said.
var legacyModes = map[Mode]bool{"default": true, "dontAsk": true}

// Resolve returns the mode a name selects, and whether it was a retired name
// mapped to ModeAutonomous (callers print a one-line notice). An unknown name
// is returned unchanged with deprecated=false; check Valid on the result.
func Resolve(m Mode) (resolved Mode, deprecated bool) {
	if legacyModes[m] {
		return ModeAutonomous, true
	}
	return m, false
}

// DeprecatedNotice is the one line shown when a retired mode name is used.
func DeprecatedNotice(m Mode) string {
	return "permission mode " + string(m) + " is retired; using autonomous (project work runs, changes to this machine still ask)"
}
