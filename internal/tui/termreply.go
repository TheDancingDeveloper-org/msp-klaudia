package tui

import (
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// Terminal replies that Bubble Tea v1 mistakes for typing.
//
// A terminal answers some queries by writing an escape sequence to our stdin:
// OSC 10/11/4 colour queries (ESC ] 11 ; rgb:… ST) and DCS status reports
// (ESC P … ST). Bubble Tea v1 has no OSC or DCS parser, so a reply reaches
// Update as ordinary keys — alt+] (or alt+P), the body as runes, then alt+\ for
// the ST terminator or ctrl+g for BEL (which is also a real binding) — and the
// textarea types them.
//
// Klaudia triggers one such query itself: the first lipgloss.AdaptiveColor
// rendered while New builds the banner makes termenv send OSC 11 followed by a
// cursor-position request as a fence, and read the replies. termenv gives up at
// the first reply that is not OSC, or after five seconds. A terminal that
// answers the fence first — a PTY relaying OSC 11 to a frontend and answering
// CPR locally — leaves the OSC 11 reply in stdin for Bubble Tea, which is how
// "]11;rgb:ffff/ffff/ffff\" ended up in the prompt. Cursor position reports
// need nothing here: they arrive as Bubble Tea's unknown-CSI message, which
// nothing handles.

// termReplyMaxBody bounds how much is swallowed waiting for a terminator, so a
// genuine Alt chord followed by a long run of digits cannot eat input forever.
const termReplyMaxBody = 512

type termReplyFilter struct {
	held tea.KeyMsg // the alt+] or alt+P that may open a reply
	// state: 0 idle, 1 introducer held, 2 inside a reply body.
	state int
	body  int
}

// filter returns the keys to deliver for k: none while a reply is being
// swallowed, and two when a held introducer turns out to be a real keystroke.
func (f *termReplyFilter) filter(k tea.KeyMsg) []tea.KeyMsg {
	switch f.state {
	case 1:
		f.state = 0
		if k.Type == tea.KeyRunes && !k.Alt && len(k.Runes) > 0 && opensReplyBody(f.held.Runes[0], k.Runes[0]) {
			f.state, f.body = 2, len(k.Runes)
			return nil
		}
		return []tea.KeyMsg{f.held, k}
	case 2:
		switch {
		case k.Type == tea.KeyCtrlG, k.Alt && k.Type == tea.KeyRunes && string(k.Runes) == `\`:
			f.state = 0 // BEL or ST: the reply is complete
			return nil
		case k.Type == tea.KeyRunes && !k.Alt, k.Type == tea.KeySpace:
			if f.body += len(k.Runes); f.body <= termReplyMaxBody {
				return nil
			}
		}
		// Anything else — Enter, an arrow, a runaway body — was never part of
		// a reply. What was swallowed is gone; this key is not.
		f.state = 0
		return []tea.KeyMsg{k}
	}
	if k.Type == tea.KeyRunes && k.Alt && len(k.Runes) == 1 && (k.Runes[0] == ']' || k.Runes[0] == 'P') {
		f.held, f.state = k, 1
		return nil
	}
	return []tea.KeyMsg{k}
}

// opensReplyBody reports whether r can start the body of a reply introduced by
// intro: an OSC reply starts with its numeric code, a DCS reply with a digit or
// one of the intermediate/private markers terminals use (DECRQSS "1$r",
// XTVERSION ">|", XTGETTCAP "1+r").
func opensReplyBody(intro, r rune) bool {
	if unicode.IsDigit(r) {
		return true
	}
	return intro == 'P' && (r == '>' || r == '$' || r == '+' || r == '!' || r == '|')
}

// onKeys delivers the keys the reply filter lets through, in order.
func (m *Model) onKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	keys := m.termReply.filter(msg)
	if len(keys) == 1 {
		return m.onKey(keys[0])
	}
	var cmds []tea.Cmd
	for _, k := range keys {
		_, cmd := m.onKey(k)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}
