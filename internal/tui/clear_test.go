package tui

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// /clear starts a new session, so the cleared conversation is not what the
// next launch auto-resumes, and says where the old one went.
func TestClearRotatesTheSession(t *testing.T) {
	m := newPasteModel(t)
	m.sess.SessionID = "old"
	rotations := 0
	m.sess.Rotate = func() (string, string) {
		rotations++
		return "new", "old"
	}
	m.history = []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi"))}

	m.handleSlash("/clear")

	if rotations != 1 {
		t.Fatalf("Rotate called %d times, want 1", rotations)
	}
	if m.sess.SessionID != "new" {
		t.Errorf("SessionID = %q, want new", m.sess.SessionID)
	}
	if len(m.history) != 0 {
		t.Error("/clear left history in memory")
	}
	if !strings.Contains(m.transcript.String(), "klaudia --resume old") {
		t.Errorf("transcript does not say how to reopen the cleared session:\n%s", m.transcript.String())
	}
}

// Nothing recorded, nothing to reopen: no resume hint.
func TestClearOfAnEmptySessionOffersNoResume(t *testing.T) {
	m := newPasteModel(t)
	m.sess.Rotate = func() (string, string) { return "new", "" }
	m.handleSlash("/clear")
	if strings.Contains(m.transcript.String(), "--resume") {
		t.Errorf("resume hint for a session that recorded nothing:\n%s", m.transcript.String())
	}
}

// A refused /clear (mid-turn) must not rotate either: the turn in flight is
// still recording into the current session.
func TestBusyClearDoesNotRotate(t *testing.T) {
	m := newPasteModel(t)
	m.state = stateRunning
	m.sess.Rotate = func() (string, string) {
		t.Error("Rotate called while a turn was running")
		return "new", ""
	}
	m.handleSlash("/clear")
}
