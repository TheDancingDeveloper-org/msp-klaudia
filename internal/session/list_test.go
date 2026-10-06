package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTranscript lays down a transcript with the given JSONL lines and mtime.
func writeTranscript(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func userLine(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	return string(b)
}

func userBlocksLine(blocks ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": blocks},
	})
	return string(b)
}

func TestList(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	now := time.Now()

	writeTranscript(t, Path(cwd, "old"), now.Add(-2*time.Hour), userLine("the older question"))
	writeTranscript(t, Path(cwd, "new"), now.Add(-time.Minute), userLine("the newer question"))

	got := List(cwd)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(got), got)
	}
	// Newest first, which is the order a picker wants and the order MostRecent
	// depends on.
	if got[0].ID != "new" || got[1].ID != "old" {
		t.Errorf("ids = %q,%q, want new,old", got[0].ID, got[1].ID)
	}
	if got[0].Title != "the newer question" {
		t.Errorf("title = %q", got[0].Title)
	}
	if got[0].Path != Path(cwd, "new") {
		t.Errorf("path = %q, want %q", got[0].Path, Path(cwd, "new"))
	}
	if got[0].Modified.IsZero() {
		t.Error("no mtime")
	}
}

func TestListSkipsFilesWithNoConversation(t *testing.T) {
	// A launch creates its transcript eagerly but only records on a real turn,
	// so an abandoned session leaves a file with the newest mtime. Offering it
	// as something to resume is offering an empty conversation.
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	now := time.Now()

	writeTranscript(t, Path(cwd, "real"), now.Add(-time.Hour), userLine("a real turn"))
	writeTranscript(t, Path(cwd, "zero"), now)
	// Non-empty but holding no message: a summary-only file, which is what
	// compaction leaves when the session was never used afterwards.
	writeTranscript(t, Path(cwd, "summary-only"), now,
		`{"type":"summary","summary":"something"}`)

	got := List(cwd)
	if len(got) != 1 || got[0].ID != "real" {
		t.Fatalf("got %+v, want only the session with a conversation", ids(got))
	}
}

func TestListTitles(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name:  "a string content is the title",
			lines: []string{userLine("what is in b.go?")},
			want:  "what is in b.go?",
		},
		{
			name: "a block list uses its first text block",
			// Both shapes are on disk: the recorder stores whatever the API
			// took, and that is a bare string on some paths and blocks on
			// others.
			lines: []string{userBlocksLine(
				map[string]any{"type": "text", "text": "explain the gate"},
			)},
			want: "explain the gate",
		},
		{
			name: "a tool result opening the turn is skipped for the text",
			lines: []string{userBlocksLine(
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1"},
				map[string]any{"type": "text", "text": "and now?"},
			)},
			want: "and now?",
		},
		{
			name:  "newlines and runs of spaces collapse",
			lines: []string{userLine("fix\n\tthe   parser\nplease")},
			want:  "fix the parser please",
		},
		{
			name: "a transcript opening with an assistant turn has no title",
			// A replayed compaction summary opens this way. It is still a
			// conversation, so it must be listed — with no title rather than
			// dropped.
			lines: []string{`{"type":"assistant","message":{"role":"assistant","content":"resuming"}}`},
			want:  "",
		},
		{
			name: "a non-text prompt has no title",
			lines: []string{userBlocksLine(
				map[string]any{"type": "image", "source": map[string]any{"data": "AAAA"}},
			)},
			want: "",
		},
		{
			name: "an unparseable line is stepped over",
			// A half-written line from a killed process must not hide the
			// conversation underneath it.
			lines: []string{"{not json", userLine("still here")},
			want:  "still here",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
			cwd := "/work/proj"
			writeTranscript(t, Path(cwd, "s"), time.Time{}, tc.lines...)
			got := List(cwd)
			if len(got) != 1 {
				t.Fatalf("got %d sessions, want 1", len(got))
			}
			if got[0].Title != tc.want {
				t.Errorf("title = %q, want %q", got[0].Title, tc.want)
			}
		})
	}
}

func TestListTitleIsBounded(t *testing.T) {
	// A prompt can be a whole pasted file. A picker entry cannot.
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	writeTranscript(t, Path(cwd, "s"), time.Time{}, userLine(strings.Repeat("ab", 400)))
	got := List(cwd)
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1", len(got))
	}
	if n := len([]rune(got[0].Title)); n != maxTitle {
		t.Errorf("title length = %d, want %d", n, maxTitle)
	}
	if !strings.HasSuffix(got[0].Title, "…") {
		t.Errorf("a truncated title should say so: %q", got[0].Title)
	}
}

func TestListIncludesLegacyRootAndPrefersTheCurrentOne(t *testing.T) {
	// Both roots can hold the same id mid-migration. Listing it twice would
	// show the user one conversation as two.
	root := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", root)
	cwd := "/work/proj"
	legacyPath := func(id string) string {
		return filepath.Join(root, "projects", EncodePath(cwd), id+".jsonl")
	}

	writeTranscript(t, legacyPath("shared"), time.Time{}, userLine("the legacy copy"))
	writeTranscript(t, Path(cwd, "shared"), time.Time{}, userLine("the current copy"))
	writeTranscript(t, legacyPath("legacy-only"), time.Time{}, userLine("only in the old root"))

	got := List(cwd)
	if len(got) != 2 {
		t.Fatalf("got %v, want two distinct sessions", ids(got))
	}
	byID := map[string]Info{}
	for _, i := range got {
		byID[i.ID] = i
	}
	if byID["shared"].Title != "the current copy" {
		t.Errorf("shared title = %q, want the current root's copy", byID["shared"].Title)
	}
	if byID["legacy-only"].Title != "only in the old root" {
		t.Errorf("legacy-only is missing from %v", ids(got))
	}
}

func TestListOfAProjectWithNoSessions(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	if got := List("/work/nothing-here"); len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

func TestMostRecentAgreesWithList(t *testing.T) {
	// MostRecent used to walk the directories itself, and the two traversals
	// disagreed: one skipped contentless files and the other did not, so the
	// picker and auto-resume could land on different sessions.
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	now := time.Now()
	writeTranscript(t, Path(cwd, "older"), now.Add(-time.Hour), userLine("first"))
	writeTranscript(t, Path(cwd, "newer"), now.Add(-time.Minute), userLine("second"))
	writeTranscript(t, Path(cwd, "ghost"), now)

	id, ok := MostRecent(cwd)
	if !ok {
		t.Fatal("MostRecent found nothing")
	}
	all := List(cwd)
	if len(all) == 0 || all[0].ID != id {
		t.Errorf("MostRecent = %q, List's newest = %v", id, ids(all))
	}
}

func ids(in []Info) []string {
	out := make([]string, 0, len(in))
	for _, i := range in {
		out = append(out, i.ID)
	}
	return out
}
