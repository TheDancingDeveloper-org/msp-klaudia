package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func msg(role, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"role":    role,
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	return b
}

func texts(t *testing.T, entries []Entry) []string {
	t.Helper()
	var out []string
	for _, e := range entries {
		var m struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(e.Message, &m); err != nil || len(m.Content) == 0 {
			t.Fatalf("entry %s: %v", e.UUID, err)
		}
		out = append(out, m.Content[0].Text)
	}
	return out
}

// The messages after the last boundary are the ones no summary covers.
func TestReadSinceCompactionReturnsMessagesAfterLastBoundary(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	tr, err := NewTranscript(Meta{SessionID: "s1", CWD: "/work/proj"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	for _, step := range []func() error{
		func() error { return tr.Record("user", msg("user", "one")) },
		func() error { return tr.Record("assistant", msg("assistant", "two")) },
		tr.MarkCompaction,
		func() error { return tr.Record("user", msg("user", "three")) },
		func() error { return tr.Record("assistant", msg("assistant", "four")) },
		tr.MarkCompaction,
		func() error { return tr.Record("user", msg("user", "five")) },
		func() error { return tr.Record("assistant", msg("assistant", "six")) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	tail, marked, err := ReadSinceCompaction(tr.Path())
	if err != nil || !marked {
		t.Fatalf("ReadSinceCompaction: marked=%v err=%v", marked, err)
	}
	if got := strings.Join(texts(t, tail), ","); got != "five,six" {
		t.Errorf("after last boundary = %s, want five,six", got)
	}

	// Read, and so --full replay and every other caller, skips the boundary.
	all, err := Read(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(texts(t, all), ","); got != "one,two,three,four,five,six" {
		t.Errorf("Read = %s, want every message", got)
	}

	// On disk it is the reference's shape: a system entry with that subtype.
	raw, _ := os.ReadFile(tr.Path())
	if !strings.Contains(string(raw), `"type":"system","subtype":"compact_boundary"`) {
		t.Errorf("no compact_boundary line in transcript:\n%s", raw)
	}
}

// A boundary with nothing after it: the summary is the whole history.
func TestReadSinceCompactionBoundaryIsLast(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	tr, _ := NewTranscript(Meta{SessionID: "s2", CWD: "/work/proj"})
	defer tr.Close()
	if err := tr.Record("user", msg("user", "one")); err != nil {
		t.Fatal(err)
	}
	if err := tr.MarkCompaction(); err != nil {
		t.Fatal(err)
	}
	tail, marked, err := ReadSinceCompaction(tr.Path())
	if err != nil || !marked || len(tail) != 0 {
		t.Fatalf("tail=%d marked=%v err=%v, want 0 true nil", len(tail), marked, err)
	}
}

// A transcript written before boundaries existed reports no marker.
func TestReadSinceCompactionWithoutBoundary(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	tr, _ := NewTranscript(Meta{SessionID: "s3", CWD: "/work/proj"})
	defer tr.Close()
	if err := tr.Record("user", msg("user", "one")); err != nil {
		t.Fatal(err)
	}
	if _, marked, err := ReadSinceCompaction(tr.Path()); err != nil || marked {
		t.Fatalf("marked=%v err=%v, want false nil", marked, err)
	}
}
