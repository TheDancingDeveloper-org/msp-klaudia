package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// userMessage builds a recorded user message with a single text block, matching
// the shape record() persists.
func userMessage(t *testing.T, text string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seedSessionWith writes a transcript for id under cwd with a first user prompt
// and stamps its mtime, so listing and retention have something deterministic
// to work with.
func seedSessionWith(t *testing.T, cwd, id, prompt string, mod time.Time) {
	t.Helper()
	w, err := NewWriter(cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Entry{Type: "user", SessionID: id, CWD: cwd, Message: userMessage(t, prompt)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(Path(cwd, id), mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeriveTitleFromFirstPrompt(t *testing.T) {
	entries := []Entry{
		{Type: "assistant", Message: userMessage(t, "ignored")},
		{Type: "user", Message: userMessage(t, "Fix the JSON parser bug\nand add a test")},
	}
	if got := DeriveTitle(entries); got != "Fix the JSON parser bug" {
		t.Fatalf("DeriveTitle = %q, want first line only", got)
	}
}

func TestDeriveTitleTruncatesLongPrompt(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := DeriveTitle([]Entry{{Type: "user", Message: userMessage(t, long)}})
	if r := []rune(got); len(r) != maxTitleLen+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("DeriveTitle len = %d (%q), want %d runes + ellipsis", len([]rune(got)), got, maxTitleLen)
	}
}

func TestDeriveTitleSkipsToolResultUserMessages(t *testing.T) {
	// A user-role message carrying only a tool_result block is the loop feeding
	// output back, not a prompt; the title should come from the real prompt.
	toolResult, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "tool_result", "content": "done"}},
	})
	entries := []Entry{
		{Type: "user", Message: toolResult},
		{Type: "user", Message: userMessage(t, "Real prompt")},
	}
	if got := DeriveTitle(entries); got != "Real prompt" {
		t.Fatalf("DeriveTitle = %q, want %q", got, "Real prompt")
	}
}

func TestSetAndReadTitle(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	seedSessionWith(t, cwd, "sid", "original prompt", time.Time{})
	if err := SetTitle(cwd, "sid", "My renamed session"); err != nil {
		t.Fatal(err)
	}
	got, ok := Title(cwd, "sid")
	if !ok || got != "My renamed session" {
		t.Fatalf("Title = (%q, %v), want My renamed session", got, ok)
	}
}

func TestListReturnsSessionsNewestFirstWithBackfilledTitles(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	now := time.Now()
	seedSessionWith(t, cwd, "older", "first task", now.Add(-2*time.Hour))
	seedSessionWith(t, cwd, "newer", "second task", now.Add(-1*time.Hour))

	infos, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("List returned %d sessions, want 2", len(infos))
	}
	if infos[0].ID != "newer" || infos[1].ID != "older" {
		t.Fatalf("List order = [%s %s], want [newer older]", infos[0].ID, infos[1].ID)
	}
	if infos[0].Title != "second task" || infos[0].Project != cwd {
		t.Fatalf("List[0] = %+v, want title/project derived", infos[0])
	}
	// The title should have been persisted (lazy backfill), not re-derived.
	if _, ok := readMeta(MetaPath(cwd, "newer")); !ok {
		t.Fatalf("expected backfilled meta sidecar for %q", "newer")
	}
}

func TestDeleteByIDRemovesTranscriptAndSidecars(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	seedSessionWith(t, cwd, "sid", "task", time.Time{})
	if err := SetTitle(cwd, "sid", "titled"); err != nil {
		t.Fatal(err)
	}
	if err := WriteSummary(cwd, "sid", "a summary", ""); err != nil {
		t.Fatal(err)
	}

	deleted, err := DeleteByID("sid")
	if err != nil || !deleted {
		t.Fatalf("DeleteByID = (%v, %v), want (true, nil)", deleted, err)
	}
	for _, p := range []string{Path(cwd, "sid"), SummaryPath(cwd, "sid"), MetaPath(cwd, "sid")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, stat err = %v", filepath.Base(p), err)
		}
	}

	// Deleting again reports "nothing to delete" rather than an error.
	deleted, err = DeleteByID("sid")
	if err != nil || deleted {
		t.Fatalf("second DeleteByID = (%v, %v), want (false, nil)", deleted, err)
	}
}

func TestDeleteByIDRejectsInvalidID(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	if _, err := DeleteByID("../escape"); err == nil {
		t.Fatal("DeleteByID(../escape) should reject an invalid id")
	}
}

func TestPruneByCountKeepsNewestAndProtectsActive(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	now := time.Now()
	// Oldest → newest: s0 (active, oldest), s1, s2, s3.
	seedSessionWith(t, cwd, "s0-active", "old active", now.Add(-4*time.Hour))
	seedSessionWith(t, cwd, "s1", "one", now.Add(-3*time.Hour))
	seedSessionWith(t, cwd, "s2", "two", now.Add(-2*time.Hour))
	seedSessionWith(t, cwd, "s3", "three", now.Add(-1*time.Hour))

	removed, err := Prune(Retention{MaxCount: 2}, "s0-active")
	if err != nil {
		t.Fatal(err)
	}
	// Active is off the count and untouchable; of the remaining three the two
	// newest (s3, s2) are kept and the oldest non-active (s1) is pruned.
	if len(removed) != 1 || removed[0] != "s1" {
		t.Fatalf("removed = %v, want [s1]", removed)
	}
	if _, err := os.Stat(Path(cwd, "s0-active")); err != nil {
		t.Fatalf("active session must survive, stat err = %v", err)
	}
	if _, err := os.Stat(Path(cwd, "s1")); !os.IsNotExist(err) {
		t.Fatalf("expected s1 pruned, stat err = %v", err)
	}
}

func TestPruneByAge(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	now := time.Now()
	seedSessionWith(t, cwd, "fresh", "recent", now.Add(-1*time.Hour))
	seedSessionWith(t, cwd, "stale", "ancient", now.Add(-48*time.Hour))

	removed, err := Prune(Retention{MaxAge: 24 * time.Hour}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "stale" {
		t.Fatalf("removed = %v, want [stale]", removed)
	}
	if _, err := os.Stat(Path(cwd, "fresh")); err != nil {
		t.Fatalf("fresh session must survive, stat err = %v", err)
	}
}

func TestPruneNoPolicyIsNoop(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	seedSessionWith(t, cwd, "sid", "task", time.Now().Add(-1000*time.Hour))
	removed, err := Prune(Retention{}, "")
	if err != nil || len(removed) != 0 {
		t.Fatalf("Prune with no caps = (%v, %v), want ([], nil)", removed, err)
	}
	if _, err := os.Stat(Path(cwd, "sid")); err != nil {
		t.Fatalf("no-op prune must not delete, stat err = %v", err)
	}
}
