package tui

import (
	"strings"
	"testing"
)

func summaryModel(t *testing.T) *Model {
	t.Helper()
	m := newTestModel()
	m.resize(100, 40)
	return m
}

func TestSummaryNoneYet(t *testing.T) {
	m := summaryModel(t)
	m.handleSlash("/summary")
	out := visibleText(m.transcript.String())
	if !strings.Contains(out, "No compaction summary yet") {
		t.Errorf("want a 'none yet' notice, got:\n%s", out)
	}
}

func TestSummaryShowsInMemory(t *testing.T) {
	m := summaryModel(t)
	m.lastSummary = "we refactored the auth token refresh"
	m.handleSlash("/summary")
	out := visibleText(m.transcript.String())
	if !strings.Contains(out, "auth token refresh") {
		t.Errorf("want the in-memory summary shown, got:\n%s", out)
	}
}

func TestSummaryFallsBackToPersisted(t *testing.T) {
	m := summaryModel(t)
	// No in-memory summary this session; the persisted one from an earlier
	// session must still be shown.
	m.sess.ReadSummary = func() (string, bool) { return "persisted from last time", true }
	m.handleSlash("/summary")
	out := visibleText(m.transcript.String())
	if !strings.Contains(out, "persisted from last time") {
		t.Errorf("want the persisted summary shown, got:\n%s", out)
	}
}

func TestSummaryPrefersInMemoryOverPersisted(t *testing.T) {
	m := summaryModel(t)
	m.lastSummary = "fresh this session"
	m.sess.ReadSummary = func() (string, bool) { return "stale on disk", true }
	got, ok := m.currentSummary()
	if !ok || got != "fresh this session" {
		t.Errorf("currentSummary = (%q, %v), want the fresher in-memory summary", got, ok)
	}
}

func TestSummaryEditWithoutStoreShowsInstead(t *testing.T) {
	m := summaryModel(t)
	m.lastSummary = "something to edit"
	m.sess.SaveSummary = nil // no persistence wired
	m.handleSlash("/summary edit")
	out := visibleText(m.transcript.String())
	if !strings.Contains(out, "not available") {
		t.Errorf("want an unavailable notice when editing has nowhere to persist, got:\n%s", out)
	}
	// It still shows the summary so the user can copy it out.
	if !strings.Contains(out, "something to edit") {
		t.Errorf("edit-unavailable should still print the summary, got:\n%s", out)
	}
}
