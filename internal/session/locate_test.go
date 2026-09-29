package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocatePrefersCWDThenSearchesTheRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", root)

	other := Path("/work/a", "s1")
	writeTranscript(t, other)
	if got, ok := Locate("/work/b", "s1"); !ok || got != other {
		t.Fatalf("Locate from another cwd = %q, %v; want %q", got, ok, other)
	}

	// A copy under cwd wins even when another dir's copy is newer.
	own := Path("/work/b", "s1")
	writeTranscript(t, own)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(other, later, later); err != nil {
		t.Fatal(err)
	}
	if got, _ := Locate("/work/b", "s1"); got != own {
		t.Fatalf("Locate = %q, want cwd copy %q", got, own)
	}
	// From a third dir, the newest copy wins.
	if got, _ := Locate("/work/c", "s1"); got != other {
		t.Fatalf("Locate = %q, want newest copy %q", got, other)
	}

	legacy := filepath.Join(root, "projects", EncodePath("/old"), "s2.jsonl")
	writeTranscript(t, legacy)
	if got, ok := Locate("/work/c", "s2"); !ok || got != legacy {
		t.Fatalf("Locate legacy = %q, %v; want %q", got, ok, legacy)
	}
	if _, ok := Locate("/work/c", "missing"); ok {
		t.Fatal("Locate found a session that does not exist")
	}
}

func TestValidIDRejectsPathLikeIDs(t *testing.T) {
	for _, id := range []string{"", "..", "../x", "a/b", `a\b`, ".hidden", "x.summary", "a b"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
		if _, ok := Locate("/w", id); ok {
			t.Errorf("Locate(%q) found something", id)
		}
	}
	for _, id := range []string{"3f1c2a9e-5b7d-4e0a-9c1b-2d3e4f5a6b7c", "0123456789abcdef0123456789abcdef", "chat_42"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
}

func TestSummaryFollowsTheLocatedTranscript(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	tp := Path("/work/a", "s1")
	writeTranscript(t, tp)
	if err := WriteSummaryAt(SummaryPathFor(tp), "s1", "the summary", ""); err != nil {
		t.Fatal(err)
	}
	if got, ok := ReadSummary("/work/b", "s1"); !ok || got != "the summary" {
		t.Fatalf("ReadSummary from another cwd = %q, %v", got, ok)
	}
}

// MostRecent reads every key it is given — the project root and the launch
// directory — and returns the newest transcript across them.
func TestMostRecentAcrossKeys(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	root, cwd := "/work/proj", "/work/proj/sub"
	writeTranscript(t, Path(cwd, "old-cwd-keyed"))
	if id, ok := MostRecent(root, cwd); !ok || id != "old-cwd-keyed" {
		t.Fatalf("MostRecent = %q, %v; want the cwd-keyed session", id, ok)
	}
	if _, ok := MostRecent(root); ok {
		t.Fatal("MostRecent(root) found a session only the cwd key holds")
	}
	writeTranscript(t, Path(root, "root-keyed"))
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(Path(root, "root-keyed"), future, future); err != nil {
		t.Fatal(err)
	}
	if id, _ := MostRecent(root, cwd, root); id != "root-keyed" {
		t.Fatalf("MostRecent = %q, want the newer root-keyed session", id)
	}
}
