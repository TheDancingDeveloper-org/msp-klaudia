package tui

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/memory"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// idleWindow is how long an idle program must stay silent. Three blink periods
// of the bubbles cursor (530ms) and dozens of spinner frames: long enough that
// any timer still running would have drawn something.
const idleWindow = 1600 * time.Millisecond

// wire is the terminal's side of a running program: everything written to it,
// safe to read while the renderer is still writing.
type wire struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *wire) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *wire) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// waitFor polls until the wire carries want count times, or fails.
func (w *wire) waitFor(t *testing.T, want string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(w.String(), want) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never saw %q ×%d on the wire:\n%q", want, count, w.String())
}

// quietFor reports what the program wrote during d, starting once the wire has
// settled — 100ms with no new bytes, so the frame for the last state change
// has been flushed whatever the machine's speed. A program that never settles
// (a blinking cursor) is measured from the settle deadline instead.
func (w *wire) quietFor(d time.Duration) string {
	last, still := len(w.String()), time.Now()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		if n := len(w.String()); n != last {
			last, still = n, time.Now()
		} else if time.Since(still) >= 100*time.Millisecond {
			break
		}
	}
	before := len(w.String())
	time.Sleep(d)
	return w.String()[before:]
}

func titleSeq(title string) string { return ansi.SetWindowTitle(title) }

// runProgram starts the real program, renderer and all, against a wire.
func runProgram(t *testing.T, run RunFunc, sess *Session) (*tea.Program, *wire) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	if sess.Memory == nil {
		sess.Memory = memory.Disabled()
	}
	// A real terminal's colours. With none, the cursor's reverse video renders
	// as nothing, a blink changes no byte of the frame, and the renderer — which
	// skips a frame identical to the last — would hide exactly what is measured.
	saved := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	ctx, cancel := context.WithCancel(context.Background())
	out := &wire{}
	m := New(ctx, run, nil, sess)
	m.titleOut = out
	p := tea.NewProgram(m,
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(out),
		tea.WithoutSignalHandler(),
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := p.Run(); err != nil {
			t.Errorf("program: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		p.Quit()
		<-done
		lipgloss.SetColorProfile(saved)
	})
	p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	return p, out
}

func submit(p *tea.Program, text string) {
	p.Send(runeKey(text))
	p.Send(tea.KeyMsg{Type: tea.KeyEnter})
}

// releasedRun is a turn that holds until released, so a test can watch the
// running state (spinner, stopwatch) before the turn ends.
func releasedRun(release <-chan struct{}) RunFunc {
	return func(ctx context.Context, _ agent.Turn) (agent.Result, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
		return agent.Result{Text: "all done"}, nil
	}
}

// The bug: an idle prompt never stopped writing. The cursor blink repainted
// the input box twice a second, forever, so a program watching the terminal
// for quiet never saw any, and its log grew by megabytes an hour.
//
// A turn runs first, so the spinner and stopwatch have both been started and
// must have stopped — not just the cursor that was never asked to move.
func TestIdlePromptWritesNothing(t *testing.T) {
	release := make(chan struct{})
	p, out := runProgram(t, releasedRun(release), &Session{})

	out.waitFor(t, titleSeq(TitleReady), 1)
	submit(p, "hello")
	out.waitFor(t, titleSeq(TitleWorking), 1)
	time.Sleep(300 * time.Millisecond) // let the spinner and stopwatch run
	close(release)
	out.waitFor(t, titleSeq(TitleReady), 2)

	if extra := out.quietFor(idleWindow); extra != "" {
		t.Errorf("idle prompt wrote %d bytes in %s:\n%q", len(extra), idleWindow, extra)
	}

	// The title is the explicit signal, in order: ready at start, working for
	// the turn, ready again once it is over.
	raw := out.String()
	ready1 := strings.Index(raw, titleSeq(TitleReady))
	working := strings.Index(raw, titleSeq(TitleWorking))
	ready2 := strings.LastIndex(raw, titleSeq(TitleReady))
	if !(ready1 < working && working < ready2) {
		t.Errorf("titles out of order: ready@%d working@%d ready@%d", ready1, working, ready2)
	}
	if n := strings.Count(raw, titleSeq(TitleWorking)); n != 1 {
		t.Errorf("working title sent %d times; it is sent on the transition only", n)
	}
}

// The control for the test above: with the blink opted back in, the same
// measurement sees the repaints. Without this, a broken harness (a wire the
// renderer never writes to) would pass the idle test too.
func TestBlinkingCursorIsVisibleOnTheWire(t *testing.T) {
	_, out := runProgram(t, releasedRun(nil), &Session{CursorBlink: true})
	out.waitFor(t, titleSeq(TitleReady), 1)
	if extra := out.quietFor(idleWindow); extra == "" {
		t.Error("a blinking cursor wrote nothing: the idle measurement cannot see a repaint")
	}
}

// A turn parked on a permission prompt is waiting for a person exactly as an
// idle prompt is. The stopwatch keeps counting underneath, but nothing it
// drives is on screen, so the terminal has to go quiet here too — and say why.
func TestApprovalPromptIsQuietAndTitled(t *testing.T) {
	run := func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
		d := turn.Approver.Approve(ctx, agent.ApprovalRequest{ToolName: "Bash", Input: []byte(`{"command":"make"}`)})
		if d.Behavior != permission.Allow {
			return agent.Result{}, nil
		}
		return agent.Result{Text: "ran it"}, nil
	}
	p, out := runProgram(t, run, &Session{})
	out.waitFor(t, titleSeq(TitleReady), 1)
	submit(p, "build it")
	out.waitFor(t, titleSeq(TitleApproval), 1)

	if extra := out.quietFor(idleWindow); extra != "" {
		t.Errorf("approval prompt wrote %d bytes in %s:\n%q", len(extra), idleWindow, extra)
	}

	p.Send(runeKey("y"))
	out.waitFor(t, titleSeq(TitleReady), 2)
	raw := out.String()
	approval := strings.Index(raw, titleSeq(TitleApproval))
	if strings.LastIndex(raw, titleSeq(TitleWorking)) < approval {
		t.Error("answering the prompt did not return the title to working")
	}
	if strings.LastIndex(raw, titleSeq(TitleReady)) < approval {
		t.Error("the end of the turn did not return the title to ready")
	}
}

// titles drives one Update and returns the titles it wrote — none when the
// state title did not change.
func titles(m *Model, msg tea.Msg) []string {
	var out bytes.Buffer
	m.titleOut = &out
	m.Update(msg)
	var got []string
	for _, seq := range strings.SplitAfter(out.String(), "\a") {
		if t, ok := strings.CutPrefix(seq, "\x1b]2;"); ok {
			got = append(got, strings.TrimSuffix(t, "\a"))
		}
	}
	return got
}

func wantTitles(t *testing.T, step string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: titles %q, want %q", step, got, want)
	}
}

func TestTitleFollowsTheTurn(t *testing.T) {
	m := slashModel(t)
	sr := &scriptedRun{res: agent.Result{Text: "ok"}}
	m.run = sr.run

	wantTitles(t, "first frame", titles(m, tea.WindowSizeMsg{Width: 80, Height: 24}), TitleReady)
	wantTitles(t, "no change", titles(m, tea.FocusMsg{}))

	m.input.SetValue("do the thing")
	wantTitles(t, "turn start", titles(m, tea.KeyMsg{Type: tea.KeyEnter}), TitleWorking)
	done := awaitMsg(t, m.events)
	wantTitles(t, "turn end", titles(m, done), TitleReady)

	// An ask and a plan stall the turn on the user exactly as a permission does.
	m.input.SetValue("again")
	titles(m, tea.KeyMsg{Type: tea.KeyEnter})
	awaitMsg(t, m.events) // this turn's doneMsg; held back while the prompts are tested
	wantTitles(t, "permission", titles(m, permissionMsg{req: agent.ApprovalRequest{ToolName: "Bash"}, reply: make(chan permission.Decision, 1)}), TitleApproval)
	wantTitles(t, "answered", titles(m, runeKey("y")), TitleWorking)
	wantTitles(t, "question", titles(m, askMsg{question: "which?", options: []tools.AskOption{{Label: "this"}}, reply: make(chan string, 1)}), TitleApproval)
	wantTitles(t, "answered", titles(m, runeKey("1")), TitleWorking)
	wantTitles(t, "plan", titles(m, planMsg{plan: "do it", reply: make(chan bool, 1)}), TitleApproval)
}

func TestTitleIsOffWhenConfigured(t *testing.T) {
	m := slashModel(t)
	m.sess.NoTitle = true
	wantTitles(t, "first frame", titles(m, tea.WindowSizeMsg{Width: 80, Height: 24}))
	m.state = stateRunning
	wantTitles(t, "running", titles(m, tea.FocusMsg{}))
}

// /goal run spans many turns; between them it is still not the user's move, so
// the title says goal-loop until the loop ends — except while a prompt inside
// it needs a person.
func TestTitleMarksTheGoalLoop(t *testing.T) {
	dir := gitRepo(t)
	m := slashModel(t)
	m.sess.CWD = dir
	sr := &scriptedRun{res: agent.Result{Text: "made progress"}}
	m.run = sr.run
	write(t, dir, "PRD.md", "# Goal: build the widget\n\n## Progress\n\n- [ ] widget\n\n## Verify\n\nmake test\n")
	titles(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m.input.SetValue("/goal run 3")
	wantTitles(t, "loop start", titles(m, tea.KeyMsg{Type: tea.KeyEnter}), TitleGoalLoop)
	done := awaitMsg(t, m.events)
	wantTitles(t, "next iteration", titles(m, done)) // still goal-loop: no change, nothing sent
	if m.loopRemaining == 0 {
		t.Fatal("the loop ended after one iteration; the test needs it running")
	}
	awaitMsg(t, m.events) // the second iteration's doneMsg, held back

	wantTitles(t, "permission in loop", titles(m, permissionMsg{req: agent.ApprovalRequest{ToolName: "Bash"}, reply: make(chan permission.Decision, 1)}), TitleApproval)
	wantTitles(t, "answered", titles(m, runeKey("y")), TitleGoalLoop)

	wantTitles(t, "interrupted", titles(m, tea.KeyMsg{Type: tea.KeyEsc}), TitleWorking)
	wantTitles(t, "stopped", titles(m, doneMsg{err: context.Canceled}), TitleReady)
}

// The idle prompt has no spinner on screen, so it has no spinner ticking.
func TestSpinnerStopsOffScreen(t *testing.T) {
	m := slashModel(t)
	m.sess.NoTitle = true // only the spinner's command is under test
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.state = stateRunning
	if _, cmd := m.Update(tea.FocusMsg{}); cmd == nil || !m.spinning {
		t.Fatal("entering the running state did not arm the spinner")
	}
	m.state = stateIdle
	tick := m.spin.Tick()
	if _, cmd := m.Update(tick); cmd != nil {
		t.Errorf("an idle spinner tick re-armed itself (%T)", cmd())
	}
	if m.spinning {
		t.Error("spinner still marked running at an idle prompt")
	}
}

func TestCursorBlinksResolution(t *testing.T) {
	cases := []struct {
		setting, env string
		want         bool
	}{
		{"", "", false},
		{"steady", "", false},
		{"blink", "", true},
		{"blink", "0", false},
		{"", "1", true},
		{"steady", "true", true},
		{"wobble", "", false},
	}
	for _, c := range cases {
		if got := CursorBlinks(c.setting, c.env, nil); got != c.want {
			t.Errorf("CursorBlinks(%q, %q) = %v, want %v", c.setting, c.env, got, c.want)
		}
	}
	var warned []string
	CursorBlinks("wobble", "maybe", func(s string) { warned = append(warned, s) })
	if len(warned) != 2 {
		t.Errorf("want a warning each for the bad env and setting, got %q", warned)
	}
	if !TitleOff("off", nil) || TitleOff("", nil) || TitleOff("on", nil) {
		t.Error("TitleOff: off suppresses, unset and on do not")
	}
}

// A session must not leave its state in the terminal's title after it exits:
// a pane reading "klaudia: ready" over a dead process is the one wrong answer.
// The title stack is pushed at start; on exit the title is cleared and popped,
// so a terminal without the stack falls back to its default.
func TestStartTitleSavesAndRestores(t *testing.T) {
	var b bytes.Buffer
	out, restore := StartTitle(&b, true, false)
	if out == nil || b.String() != "\x1b[22;0t" {
		t.Fatalf("start wrote %q (out nil: %v); want the title-stack push", b.String(), out == nil)
	}
	b.Reset()
	restore()
	if b.String() != "\x1b]2;\x07\x1b[23;0t" {
		t.Errorf("restore wrote %q; want an empty title, then the pop", b.String())
	}

	for _, c := range []struct {
		name              string
		terminal, noTitle bool
	}{{"not a terminal", false, false}, {"titles off", true, true}} {
		b.Reset()
		out, restore := StartTitle(&b, c.terminal, c.noTitle)
		restore()
		if out != nil || b.Len() != 0 {
			t.Errorf("%s: wrote %q, out nil: %v; want nothing at all", c.name, b.String(), out == nil)
		}
	}
}
