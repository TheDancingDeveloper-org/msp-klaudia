package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/greenthread-ai/klaudia/internal/compaction"
	"github.com/greenthread-ai/klaudia/internal/session"
)

// Resuming an earlier conversation means knowing its id, and nobody remembers a
// UUID. /resume offers the recent sessions as the standard numbered picker —
// title and age, newest first — and switches this session to the chosen one in
// place, reusing the same recorder-swap the CLI --resume path relies on.

// resumePickerMax caps the picker at the digits it can select (1-9).
const resumePickerMax = 9

// startResumePicker lists recent sessions and opens the picker. It lists across
// projects but surfaces the working directory so a session from elsewhere is
// recognisable; the current session is excluded, since resuming it is a no-op.
func (m *Model) startResumePicker() (tea.Model, tea.Cmd) {
	if m.sess == nil || m.sess.Resume == nil {
		m.appendLine(bannerStyle.Render("Resume isn't available in this session."))
		return m, nil
	}
	infos, err := session.List()
	if err != nil {
		m.appendLine(errStyle.Render("resume: " + err.Error()))
		return m, nil
	}
	current := m.sess.SessionID
	shown := make([]session.SessionInfo, 0, resumePickerMax)
	for _, s := range infos {
		if s.ID == current {
			continue
		}
		shown = append(shown, s)
		if len(shown) == resumePickerMax {
			break
		}
	}
	if len(shown) == 0 {
		m.appendLine(bannerStyle.Render("No other sessions to resume."))
		return m, nil
	}
	now := time.Now()
	items := make([]choiceItem, 0, len(shown))
	for _, s := range shown {
		s := s
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		label := fmt.Sprintf("%s  ·  %s ago", title, resumeAge(now.Sub(s.Modified)))
		items = append(items, choiceItem{
			label: label,
			apply: func() string { return m.resumeSession(s.ID, title) },
		})
	}
	m.startChoice("Resume a session:", items)
	return m, nil
}

// resumeSession swaps the live recorder to the chosen session and reloads its
// conversation into the view. Runs as a picker apply, so it mutates the model
// and returns the confirmation line the picker prints.
func (m *Model) resumeSession(id, title string) string {
	history, err := m.sess.Resume(id)
	if err != nil {
		return "resume failed: " + err.Error()
	}
	m.transcript.Reset()
	m.history = history
	m.pastes.reset()
	m.results.reset()
	m.nav = nil
	m.sess.SessionID = id
	m.residentTokens = compaction.EstimateTokens(history)
	m.appendResumeRecap(history)
	if st := m.buildResumeState(); len(history) > 0 && st.hasContent() {
		m.appendLine(bannerStyle.Render(st.render()))
	}
	return "Resumed " + title
}

// renameSession sets the current session's title (persisted in its metadata).
func (m *Model) renameSession(title string) {
	if title == "" {
		m.appendLine(errStyle.Render("usage: /rename <title>"))
		return
	}
	if m.sess == nil || m.sess.SessionID == "" || m.sess.CWD == "" {
		m.appendLine(errStyle.Render("No active session to rename."))
		return
	}
	if err := session.SetTitle(m.sess.CWD, m.sess.SessionID, title); err != nil {
		m.appendLine(errStyle.Render("rename failed: " + err.Error()))
		return
	}
	m.appendLine(bannerStyle.Render("Session title set to: " + title))
}

// resumeAge renders a session's age for the picker: compact and human, matching
// how the CLI `sessions ls` reads.
func resumeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
