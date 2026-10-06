package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The bug: a literal HT advances the terminal's cursor to the next tab stop
// without painting the cells it skips. Bubble Tea writes queued scrollback
// lines directly over the rows the previous frame's prompt box, placeholder
// and status bar occupied, so every skipped cell kept that chrome. Reading a
// tab-indented Go file produced lines like
//
//	266Klaudia…                        return true
//	268-opus-5 · auto-e}
//
// — the status bar showing through the indentation. The runs were 8 and 16
// columns wide: default tab stops, which is the signature.
func TestScrollbackHasNoLiteralTabs(t *testing.T) {
	m := newTestModel()
	m.resize(100, 24)

	got := m.fitScrollback("   265\tif strings.Contains(c, h) {\n   266\t\treturn true")
	if strings.ContainsRune(got, '\t') {
		t.Errorf("a tab reached the terminal, leaving unpainted cells:\n%q", got)
	}
}

// Expansion must go to real tab stops. lipgloss's default — four spaces per
// tab regardless of column — is what baseStyle() disabled in the first place,
// because it destroys the column structure of anything tabular.
func TestTabsExpandToStopsNotFixedWidth(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"start of line", "\tx", "        x"},
		{"partial column", "ab\tx", "ab      x"},
		{"exactly on a stop", "12345678\tx", "12345678        x"},
		{"one short of a stop", "1234567\tx", "1234567 x"},
		{"consecutive", "a\t\tx", "a               x"},
		{"columns stay aligned", "NAME\tSTATUS\npod\tRunning", "NAME    STATUS\npod     Running"},
		{"no tabs is untouched", "plain text", "plain text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandTabs(tc.in, tabStop); got != tc.want {
				t.Errorf("expandTabs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Tool output is styled before it reaches here, so the text carries SGR
// sequences. Those occupy no columns, and counting them would push every
// subsequent tab to the wrong stop.
func TestAnsiSequencesDoNotShiftTabStops(t *testing.T) {
	const red, reset = "\x1b[31m", "\x1b[0m"
	got := expandTabs(red+"ab"+reset+"\tx", tabStop)
	want := red + "ab" + reset + "      x"
	if got != want {
		t.Errorf("expandTabs with SGR = %q, want %q", got, want)
	}
	if !strings.Contains(got, red) || !strings.Contains(got, reset) {
		t.Error("styling must survive expansion")
	}
}

// The second, compounding defect: ansi.StringWidth scores a tab as zero
// columns, so a tab-heavy line was measured as short, never wrapped, and the
// terminal wrapped it instead — onto rows Klaudia then never erased. Expanding
// first makes the measurement honest.
func TestTabHeavyLineIsWrappedToTheTerminalWidth(t *testing.T) {
	m := newTestModel()
	m.resize(40, 24)

	// Six tabs plus text: 48 columns once expanded, 12 by the old measure.
	got := m.fitScrollback("\t\t\t\t\t\ttail")
	for _, ln := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(ln); w >= 40 {
			t.Errorf("line of %d columns was not wrapped to under 40: %q", w, ln)
		}
	}
	if !strings.Contains(got, "\n") {
		t.Error("expected the line to be wrapped onto more than one row")
	}
}

// The clipboard and /export read m.transcript, which commit() populates
// separately from the terminal path. Tabs must still be literal there: that is
// what makes `go test` output and TSV paste correctly, and it is the reason
// baseStyle() set NoTabConversion. Fixing the display must not cost it.
func TestTranscriptKeepsLiteralTabsForTheClipboard(t *testing.T) {
	m := newTestModel()
	m.resize(100, 24)

	m.renderEvent(mkResult("Bash", "NAME\tSTATUS\npod\tRunning", false))

	out := visibleText(m.transcript.String())
	if !strings.Contains(out, "NAME\tSTATUS") {
		t.Errorf("the clipboard copy lost its tabs:\n%q", out)
	}
}

// End to end: the exact shape from the bug report — Read's "%6d\t%s" line
// numbering over a tab-indented source file.
func TestLineNumberedSourceRendersWithoutTabs(t *testing.T) {
	m := newTestModel()
	m.resize(100, 24)

	m.renderEvent(mkResult("Read", "   265\tif strings.Contains(c, h) {\n   266\t\treturn true\n", false))

	printed, _ := m.out.drainText()
	for _, ln := range strings.Split(printed, "\n") {
		if strings.ContainsRune(ln, '\t') {
			t.Errorf("tab reached scrollback: %q", ln)
		}
	}
}
