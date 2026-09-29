package cli

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/session"
)

func recordText(t *testing.T, tr *session.Transcript, role, text string) {
	t.Helper()
	var m anthropic.BetaMessageParam
	if role == "user" {
		m = anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text))
	} else {
		m = anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(text)}}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Record(role, b); err != nil {
		t.Fatal(err)
	}
}

func historyTexts(msgs []anthropic.BetaMessageParam) []string {
	var out []string
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.OfText != nil {
				out = append(out, string(m.Role)+":"+c.OfText.Text)
			}
		}
	}
	return out
}

// compactedSession writes a transcript with one compaction and a turn after
// it, plus the summary that compaction produced.
func compactedSession(t *testing.T, cwd, id string, boundary bool) {
	t.Helper()
	tr, err := session.NewTranscript(session.Meta{SessionID: id, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	recordText(t, tr, "user", "before")
	recordText(t, tr, "assistant", "old answer")
	if boundary {
		if err := tr.MarkCompaction(); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.WriteSummary(cwd, id, "SUMMARY", ""); err != nil {
		t.Fatal(err)
	}
	recordText(t, tr, "user", "after")
	recordText(t, tr, "assistant", "new answer")
}

// The turns taken after the last compaction used to be dropped: resume seeded
// the summary and nothing else. They follow the summary now.
func TestResumeFromSummaryKeepsTurnsAfterCompaction(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	compactedSession(t, cwd, "sess-a", true)

	msgs, fromSummary, err := resumeMessages(cwd, "sess-a", false)
	if err != nil || !fromSummary {
		t.Fatalf("fromSummary=%v err=%v", fromSummary, err)
	}
	got := historyTexts(msgs)
	if len(got) != 3 || !strings.Contains(got[0], "SUMMARY") ||
		got[1] != "user:after" || got[2] != "assistant:new answer" {
		t.Errorf("history = %q, want the summary then after/new answer", got)
	}
	for _, s := range got {
		if strings.Contains(s, "before") || strings.Contains(s, "old answer") {
			t.Errorf("history replays a message the summary covers: %q", got)
		}
	}
}

// End to end: a launch whose tiny context window forces an autocompact before
// its answer, then a resume. The compaction's boundary is written from the
// CLI's summary hook, so the answer given after it reaches the resumed model.
func TestResumeAfterAutocompactCarriesTheAnswerAfterIt(t *testing.T) {
	fake := &fakeChat{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	dir := workdir(t, srv.URL)
	cfgPath := filepath.Join(dir, ".klaudia", "config.toml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Any window this small puts the compact threshold below zero, so the
	// first turn compacts: request 1 is the summary, request 2 the answer.
	if err := os.WriteFile(cfgPath, append(append([]byte{}, cfg...), "contextWindow = 100\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := embed(t, dir, []string{"--session-id", "compacted"}, "remember the word pineapple"); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if _, ok := session.ReadSummary(dir, "compacted"); !ok {
		t.Fatal("the first launch did not compact; the test needs it to")
	}

	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := embed(t, dir, []string{"--resume", "compacted"}, "what was the word?")
	if err != nil {
		t.Fatalf("resume launch: %v", err)
	}
	if n := lines[0]["history_messages"]; n != float64(2) {
		t.Errorf("history_messages = %v, want 2 (the summary and the answer after it)", n)
	}
	body := fake.last()
	if !strings.Contains(body, "reply-1") {
		t.Error("resumed request lacks the summary (reply-1)")
	}
	if !strings.Contains(body, "reply-2") {
		t.Error("resumed request lacks the answer given after the compaction (reply-2)")
	}
}

// --full still replays the whole transcript.
func TestResumeFullIgnoresBoundary(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	compactedSession(t, cwd, "sess-b", true)

	msgs, fromSummary, err := resumeMessages(cwd, "sess-b", true)
	if err != nil || fromSummary {
		t.Fatalf("fromSummary=%v err=%v", fromSummary, err)
	}
	if got := historyTexts(msgs); len(got) != 4 {
		t.Errorf("history = %q, want all four messages", got)
	}
}

// A transcript from before boundaries were written has no cut point, so it
// resumes from the summary alone, as it did.
func TestResumeFromSummaryWithoutBoundary(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	compactedSession(t, cwd, "sess-c", false)

	msgs, fromSummary, err := resumeMessages(cwd, "sess-c", false)
	if err != nil || !fromSummary {
		t.Fatalf("fromSummary=%v err=%v", fromSummary, err)
	}
	if got := historyTexts(msgs); len(got) != 1 || !strings.Contains(got[0], "SUMMARY") {
		t.Errorf("history = %q, want the summary alone", got)
	}
}
