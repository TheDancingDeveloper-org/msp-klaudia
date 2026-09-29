package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/session"
)

var testUserMsg = json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}]}`)

// /clear used to reset only the in-memory history: the recorder kept
// appending to the same transcript and its summary stayed, so the next launch
// auto-resumed what had been cleared.
func TestRotateStartsANewSessionAndLeavesTheOldOneResumable(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	rec := newSessionRecorder(session.Meta{SessionID: "old", CWD: cwd})
	t.Cleanup(func() { _ = rec.Close() })

	if err := rec.Record("user", testUserMsg); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteSummaryAt(rec.SummaryPath(), rec.ID(), "the old summary", ""); err != nil {
		t.Fatal(err)
	}

	newID, prevID := rec.Rotate()
	if prevID != "old" {
		t.Errorf("prevID = %q, want old", prevID)
	}
	if newID == "" || newID == "old" || rec.ID() != newID {
		t.Fatalf("newID = %q, ID() = %q; want a fresh id", newID, rec.ID())
	}
	if err := rec.Record("user", testUserMsg); err != nil {
		t.Fatal(err)
	}

	oldEntries, _ := session.Read(session.Path(cwd, "old"))
	if len(oldEntries) != 1 {
		t.Errorf("old transcript has %d messages, want 1: the new session must not append to it", len(oldEntries))
	}
	newEntries, _ := session.Read(session.Path(cwd, newID))
	if len(newEntries) != 1 || newEntries[0].SessionID != newID {
		t.Errorf("new transcript = %+v, want one message stamped %s", newEntries, newID)
	}
	if s, ok := session.ReadSummary(cwd, "old"); !ok || s != "the old summary" {
		t.Errorf("old summary = %q, %v; it should stay with the old session", s, ok)
	}
	if _, ok := session.ReadSummary(cwd, newID); ok {
		t.Error("the new session inherited a summary")
	}
	if rec.SummaryPath() != session.SummaryPath(cwd, newID) {
		t.Errorf("SummaryPath = %s, want the new session's", rec.SummaryPath())
	}
	// The new session, having said something, is what auto-resume picks.
	if id, _ := session.MostRecent(cwd); id != newID {
		t.Errorf("MostRecent = %q, want %q", id, newID)
	}
	if why := autoResumeVeto(cwd, newID); why != "" {
		t.Errorf("auto-resume of the new session vetoed: %s", why)
	}
}

// Clearing and quitting without another word leaves the cleared transcript as
// the newest one in the folder. Auto-resume must start fresh rather than bring
// it back; an explicit --resume still reopens it.
func TestAutoResumeStartsFreshAfterClearThenQuit(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	rec := newSessionRecorder(session.Meta{SessionID: "old", CWD: cwd})
	if err := rec.Record("user", testUserMsg); err != nil {
		t.Fatal(err)
	}
	newID, _ := rec.Rotate()
	_ = rec.Close()

	if _, err := os.Stat(session.Path(cwd, newID)); !os.IsNotExist(err) {
		t.Error("a new session that recorded nothing left a transcript behind")
	}
	id, ok := session.MostRecent(cwd)
	if !ok || id != "old" {
		t.Fatalf("MostRecent = %q, %v; want the cleared session", id, ok)
	}
	if why := autoResumeVeto(cwd, id); !strings.Contains(why, "cleared") || !strings.Contains(why, "--resume old") {
		t.Errorf("veto = %q, want a cleared notice naming --resume old", why)
	}
}

// Clearing a session that never said anything has nothing to leave behind.
func TestRotateOfAnEmptySessionReportsNoPrevious(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	rec := newSessionRecorder(session.Meta{SessionID: "empty", CWD: "/work/proj"})
	t.Cleanup(func() { _ = rec.Close() })
	if _, prevID := rec.Rotate(); prevID != "" {
		t.Errorf("prevID = %q, want empty", prevID)
	}
}

// A session resumed from another directory appends to the file it was found
// in; the session /clear starts belongs to this directory instead.
func TestRotateMovesAResumedElsewhereSessionHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", root)
	cwd := "/work/proj"
	elsewhere := filepath.Join(root, "sessions", "other", "old.jsonl")
	rec := newSessionRecorder(session.Meta{SessionID: "old", CWD: cwd, Path: elsewhere})
	t.Cleanup(func() { _ = rec.Close() })
	if rec.SummaryPath() != session.SummaryPathFor(elsewhere) {
		t.Errorf("SummaryPath = %s, want beside %s", rec.SummaryPath(), elsewhere)
	}
	newID, _ := rec.Rotate()
	if err := rec.Record("user", testUserMsg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session.Path(cwd, newID)); err != nil {
		t.Errorf("new session not recorded in the working directory's dir: %v", err)
	}
}
