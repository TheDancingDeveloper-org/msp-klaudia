package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// An idle prompt writes nothing.
//
// A program that runs Klaudia in a pseudo-terminal — a multiplexer, a remote
// viewer, an orchestrator — has two ways to tell "waiting for you" from
// "working": output going quiet, and the window title. The first used to be
// useless, because the input cursor blinked: a repaint of the whole input box
// about twice a second, for as long as the prompt waited. An idle session never
// went quiet, so a host waiting for quiet waited forever, and its log of the
// terminal grew by megabytes an hour of nothing happening.
//
// So nothing at the prompt runs on a timer. The cursor is steady unless the
// user opts into blinking ([tui] cursor = "blink", KLAUDIA_CURSOR_BLINK); the
// spinner's tick loop runs only while the running state draws it; the
// stopwatch stops with the turn. What remains is follow mode (/jobs follow),
// which polls a running job and prints only what the job wrote — output the
// user asked to watch, not a repaint.
//
// The second signal is explicit: the title, set with OSC 2 on every change of
// state and never otherwise. docs/embedding.md lists the strings as a contract.

// The terminal titles, exported so the headless goal loop (internal/cli)
// announces itself with the same string. A host matches on these exact strings
// (docs/embedding.md, "The interactive TUI in a terminal"), so changing one is
// a breaking change to that contract.
const (
	TitleReady    = "klaudia: ready"
	TitleWorking  = "klaudia: working"
	TitleApproval = "klaudia: awaiting approval"
	TitleGoalLoop = "klaudia: goal-loop"
)

// stateTitle is the title for the session's state now.
//
// Anything that stalls a turn on the user is "awaiting approval", whatever else
// is going on: a goal loop parked on a permission prompt needs a person exactly
// as much as an ordinary turn does. A goal loop otherwise outranks "working"
// because it spans many turns and, between them, is still not the user's move.
// A picker or confirmation the user opened from the prompt is still "ready" —
// they are the ones at the keyboard.
func (m *Model) stateTitle() string {
	switch m.state {
	case stateAwaitingPermission, stateAwaitingAnswer, stateAnsweringOther, stateAwaitingPlan:
		return TitleApproval
	}
	switch {
	case m.loopRemaining > 0 || m.loopWrapUp:
		return TitleGoalLoop
	case m.turnInFlight || m.state == stateRunning:
		return TitleWorking
	}
	return TitleReady
}

// syncTitle writes the title escape when the state title has changed. Called
// once per Update, so every transition is seen and none is sent twice.
//
// Written here, synchronously, rather than queued. Through View, like the
// clipboard and notification escapes, it would be lost: the renderer keeps only
// the latest frame between flushes, so an escape riding on one frame vanishes
// whenever a second Update lands before the flush — at startup and at the end
// of a turn, the usual case. As a tea.Cmd it would be reordered: commands run
// on their own goroutines, so "working" could arrive after the "ready" that
// followed it and leave the wrong title standing. Updates run one at a time,
// so writing from one keeps the order; the write is a single call, which
// os.File serialises against the renderer's own.
func (m *Model) syncTitle() {
	if m.titleOut == nil || (m.sess != nil && m.sess.NoTitle) {
		return
	}
	t := m.stateTitle()
	if t == m.title {
		return
	}
	m.title = t
	_, _ = io.WriteString(m.titleOut, ansi.SetWindowTitle(t)) // OSC 2 ; title BEL
}

// syncSpinner arms the spinner's tick loop when the running state has come
// back and the loop is not already going. The TickMsg handler lets the loop
// lapse whenever the spinner is off screen.
func (m *Model) syncSpinner() tea.Cmd {
	if m.state != stateRunning || m.spinning {
		return nil
	}
	m.spinning = true
	return m.spin.Tick
}

// blinkCmd starts the cursor blinking when the user asked for a blinking
// cursor, and is nil otherwise.
func (m *Model) blinkCmd() tea.Cmd {
	if m.sess == nil || !m.sess.CursorBlink {
		return nil
	}
	return textarea.Blink
}

// CursorBlinks resolves [tui] cursor and KLAUDIA_CURSOR_BLINK (env, which wins
// when set) into whether the input cursor blinks. The default is steady.
func CursorBlinks(setting, env string, warn func(string)) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "":
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		if warn != nil {
			warn(fmt.Sprintf("unknown KLAUDIA_CURSOR_BLINK %q; ignoring it (valid: 1, 0)", env))
		}
	}
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "", "steady":
		return false
	case "blink":
		return true
	default:
		if warn != nil {
			warn(fmt.Sprintf("unknown tui.cursor %q in config; using \"steady\" (valid: steady, blink)", setting))
		}
		return false
	}
}

// TitleOff resolves [tui] title into whether the state title is suppressed.
func TitleOff(setting string, warn func(string)) bool {
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "", "on":
		return false
	case "off":
		return true
	default:
		if warn != nil {
			warn(fmt.Sprintf("unknown tui.title %q in config; using \"on\" (valid: on, off)", setting))
		}
		return false
	}
}

// The title stack (XTWINOPS): 22;0 pushes the terminal's own title and 23;0
// pops it. Restoring clears the title first, so a terminal without the stack
// is left with its default title rather than the state of a session that has
// exited — a tab or tmux pane_title reading "klaudia: ready" over a dead
// process is the one wrong answer the title can give.
const (
	titlePush    = "\x1b[22;0t"
	titleRestore = "\x1b]2;\x07" + "\x1b[23;0t"
)

// StartTitle prepares out for state titles. When out is a terminal and titles
// are on, it saves the terminal's title and returns out, and a restore to call
// once the session is over; otherwise it returns nil — a pipe or a log gets no
// title escapes — and a restore that does nothing.
func StartTitle(out io.Writer, isTerminal, noTitle bool) (io.Writer, func()) {
	if out == nil || !isTerminal || noTitle {
		return nil, func() {}
	}
	_, _ = io.WriteString(out, titlePush)
	return out, func() { _, _ = io.WriteString(out, titleRestore) }
}

// GoalLoopTitle is the escape that sets the terminal title to TitleGoalLoop,
// for the headless --loop, which has no TUI to do it.
func GoalLoopTitle() string { return ansi.SetWindowTitle(TitleGoalLoop) }
