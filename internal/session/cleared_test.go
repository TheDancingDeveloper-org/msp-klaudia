package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestTranscript(t *testing.T, cwd, id string) *Transcript {
	t.Helper()
	tr, err := NewTranscript(Meta{SessionID: id, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestMarkClearedEndsTranscriptButKeepsItResumable(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	tr := newTestTranscript(t, cwd, "s1")
	msg := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}]}`)
	if err := tr.Record("user", msg); err != nil {
		t.Fatal(err)
	}

	recorded, err := tr.MarkCleared()
	if err != nil || !recorded {
		t.Fatalf("MarkCleared = %v, %v; want true, nil", recorded, err)
	}
	if !EndsCleared(tr.Path()) {
		t.Error("a transcript ending in the /clear marker was not seen as cleared")
	}
	// An explicit resume still gets the whole conversation: Read skips the marker.
	entries, err := Read(tr.Path())
	if err != nil || len(entries) != 1 || entries[0].Type != "user" {
		t.Fatalf("Read = %+v, %v; want the one user message", entries, err)
	}
	// Anything recorded after the marker means it is no longer the last act.
	if err := tr.Record("user", msg); err != nil {
		t.Fatal(err)
	}
	if EndsCleared(tr.Path()) {
		t.Error("a transcript with a message after the marker still reads as cleared")
	}
}

func TestMarkClearedLeavesNoFileForAnEmptySession(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	tr := newTestTranscript(t, "/work/proj", "empty")
	recorded, err := tr.MarkCleared()
	if err != nil || recorded {
		t.Fatalf("MarkCleared = %v, %v; want false, nil", recorded, err)
	}
	if _, err := os.Stat(tr.Path()); !os.IsNotExist(err) {
		t.Errorf("clearing a session that recorded nothing created %s", tr.Path())
	}
}

func TestEndsClearedOnlyReadsTheLastLine(t *testing.T) {
	dir := t.TempDir()
	if EndsCleared(filepath.Join(dir, "missing.jsonl")) {
		t.Error("a missing file read as cleared")
	}
	// A last line longer than the tail window is a message, never a marker.
	long := filepath.Join(dir, "long.jsonl")
	body := `{"type":"system","subtype":"clear"}` + "\n" +
		`{"type":"user","message":{"content":"` + strings.Repeat("x", 2*clearTail) + `"}}` + "\n"
	if err := os.WriteFile(long, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if EndsCleared(long) {
		t.Error("a marker followed by a long message read as cleared")
	}
}
