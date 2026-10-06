package tui

import (
	"bytes"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// quitOnF12 wraps the real Model so a test can feed raw terminal bytes through
// Bubble Tea's own input parser and know when they have all been delivered:
// F12 is appended to the input and turned into tea.Quit here, so it never
// reaches the Model and nothing is racing a timer.
type quitOnF12 struct{ m *Model }

func (q quitOnF12) Init() tea.Cmd { return nil }
func (q quitOnF12) View() string  { return q.m.View() }
func (q quitOnF12) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.Type == tea.KeyF12 {
		return q, tea.Quit
	}
	_, cmd := q.m.Update(msg)
	return q, cmd
}

// typeRaw runs raw through Bubble Tea's input reader into a fresh Model and
// returns what ended up in the prompt.
func typeRaw(t *testing.T, raw string) string {
	t.Helper()
	m := newTestModel()
	m.resize(80, 24)
	var out bytes.Buffer
	p := tea.NewProgram(quitOnF12{m},
		tea.WithInput(strings.NewReader(raw+"\x1b[24~")),
		tea.WithOutput(&out),
		tea.WithoutSignalHandler(),
	)
	if _, err := p.Run(); err != nil {
		t.Fatalf("program: %v", err)
	}
	return m.input.Value()
}

// The bug: lipgloss asks the terminal for its background colour (OSC 11) while
// Bubble Tea owns stdin. Bubble Tea v1 has no OSC parser, so the reply arrives
// as alt+], the runes "11;rgb:ffff/ffff/ffff" and alt+\ — and the prompt took
// them as typing.
func TestTerminalRepliesNeverReachThePrompt(t *testing.T) {
	cases := map[string]string{
		"OSC 11 reply, ST":    "\x1b]11;rgb:ffff/ffff/ffff\x1b\\",
		"OSC 10 reply, BEL":   "\x1b]10;rgb:0000/0000/0000\x07",
		"OSC 4 palette reply": "\x1b]4;1;rgb:cdcd/0000/0000\x1b\\",
		"cursor position":     "\x1b[24;80R",
		"DCS reply":           "\x1bP1$r0m\x1b\\",
		"OSC 11 + CPR fence":  "\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\\x1b[3;1R",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			if got := typeRaw(t, "ab"+reply+"cd"); got != "abcd" {
				t.Errorf("prompt = %q, want %q", got, "abcd")
			}
		})
	}
}

// The filter must not eat a real Alt chord that only looks like the start of
// a reply: alt+] followed by ordinary typing is delivered as it always was.
