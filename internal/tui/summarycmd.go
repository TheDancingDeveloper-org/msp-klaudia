package tui

import (
	"errors"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// /summary shows the current compaction summary and, with "edit", lets the user
// revise it in $EDITOR. The command logic lives here rather than in tui.go's
// slash switch to keep that switch a dispatcher.
//
// Why an $EDITOR round-trip and not an in-TUI textarea: the input textarea is a
// single-line prompt tuned for the wrap/last-column workarounds the project
// pins bubbles v1 for, and a compaction summary is a multi-paragraph document.
// The repo already edits files this way (/open, via editorCommand), so the same
// path — write to a temp file, hand it to $EDITOR, read it back — is the one
// with the least new surface. When no editor is set we say so rather than fall
// back to a cramped inline editor.

// errNoEditor is returned when neither $VISUAL nor $EDITOR resolves to a program
// on PATH, so /summary edit has nothing to open the summary in.
var errNoEditor = errors.New("set $EDITOR (or $VISUAL) first")

// summaryEditDoneMsg reports that the $EDITOR opened on the compaction summary
// exited, carrying the temp file to read back and remove.
type summaryEditDoneMsg struct {
	path string
	err  error
}

// currentSummary returns the summary /summary should act on: the freshest one
// produced this session, falling back to the persisted summary from an earlier
// session. ok is false when neither exists.
func (m *Model) currentSummary() (string, bool) {
	if s := strings.TrimSpace(m.lastSummary); s != "" {
		return s, true
	}
	if m.sess != nil && m.sess.ReadSummary != nil {
		if s, ok := m.sess.ReadSummary(); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// summaryCommand backs /summary. With no argument (or anything that is not
// "edit") it prints the current/last compaction summary. With "edit" it opens
// that summary in $EDITOR; the replacement happens on the editor's clean exit,
// in onSummaryEditDone.
func (m *Model) summaryCommand(args []string) (tea.Model, tea.Cmd) {
	summary, ok := m.currentSummary()
	if !ok {
		m.appendLine(bannerStyle.Render("No compaction summary yet. /compact to make one."))
		return m, nil
	}
	if len(args) == 0 || !strings.EqualFold(args[0], "edit") {
		m.appendMarkdown(summary)
		return m, nil
	}
	if m.sess == nil || m.sess.SaveSummary == nil {
		// Editing needs somewhere to persist to. Show it so the user can still
		// copy it out rather than leaving them with nothing.
		m.appendLine(errStyle.Render("summary: editing is not available in this session"))
		m.appendMarkdown(summary)
		return m, nil
	}
	cmd, err := m.editSummary(summary)
	if err != nil {
		m.appendLine(errStyle.Render("summary: " + err.Error()))
		return m, nil
	}
	m.appendLine(bannerStyle.Render("Opening the summary in your editor… save and quit to store your edits."))
	return m, cmd
}

// editSummary writes the summary to a temp file and returns a command that opens
// it in $EDITOR, tagging the exit with the temp path so it can be read back.
func (m *Model) editSummary(summary string) (tea.Cmd, error) {
	f, err := os.CreateTemp("", "klaudia-summary-*.md")
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(summary); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	f.Close()

	cmd, ok := editorCommand(os.Getenv, "", fileRef{Path: f.Name()})
	if !ok {
		os.Remove(f.Name())
		return nil, errNoEditor
	}
	path := f.Name()
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return summaryEditDoneMsg{path: path, err: err}
	}), nil
}

// onSummaryEditDone runs after $EDITOR exits: it reads the edited file, persists
// it as the new stored summary, and updates the in-memory copy. An empty result
// or a non-zero editor exit leaves the stored summary untouched — a common way
// to abandon an edit is to quit without saving.
func (m *Model) onSummaryEditDone(msg summaryEditDoneMsg) (tea.Model, tea.Cmd) {
	if msg.path != "" {
		defer os.Remove(msg.path)
	}
	if msg.err != nil {
		m.appendLine(errStyle.Render("summary: editor exited with error: " + msg.err.Error()))
		return m, nil
	}
	data, err := os.ReadFile(msg.path)
	if err != nil {
		m.appendLine(errStyle.Render("summary: could not read edited summary: " + err.Error()))
		return m, nil
	}
	edited := strings.TrimSpace(string(data))
	if edited == "" {
		m.appendLine(bannerStyle.Render("Summary unchanged (the edited file was empty)."))
		return m, nil
	}
	if edited == strings.TrimSpace(m.lastSummary) {
		m.appendLine(bannerStyle.Render("Summary unchanged."))
		return m, nil
	}
	if m.sess != nil && m.sess.SaveSummary != nil {
		if err := m.sess.SaveSummary(edited); err != nil {
			m.appendLine(errStyle.Render("summary: could not save: " + err.Error()))
			return m, nil
		}
	}
	m.lastSummary = edited
	m.appendLine(bannerStyle.Render("Saved the edited summary. It will seed a --resume of this session."))
	return m, nil
}
