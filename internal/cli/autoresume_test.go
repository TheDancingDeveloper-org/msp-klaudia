package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/greenthread-ai/klaudia/internal/session"
)

// writeSession records entries under id and sets the transcript's mtime, which
// is what auto-resume measures a session's age by.
func writeSession(t *testing.T, cwd, id string, lastActive time.Time, entries ...session.Entry) {
	t.Helper()
	w, err := session.NewWriter(cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(session.Path(cwd, id), lastActive, lastActive); err != nil {
		t.Fatal(err)
	}
}

func userMsg(text string) session.Entry {
	return session.Entry{Type: "user", Message: []byte(`{"role":"user","content":[{"type":"text","text":"` + text + `"}]}`)}
}

func assistantMsg(text string) session.Entry {
	return session.Entry{Type: "assistant", Message: []byte(`{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}`)}
}

// A recent session is resumed, and the resume is announced even when the TUI
// banner would have nothing to say (clean tree, no goal, no jobs).
func TestVetAutoResumeAnnouncesARecentSession(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd, now := "/work/proj", time.Now()
	writeSession(t, cwd, "recent", now.Add(-3*time.Hour), userMsg("hi"), assistantMsg("hello"))

	id, notice := vetAutoResume(cwd, "recent", 24*time.Hour, now)
	if id != "recent" {
		t.Fatalf("id = %q, want recent", id)
	}
	for _, want := range []string{"Resumed recent", "2 messages", "last active 3h ago", "--new-session"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q lacks %q", notice, want)
		}
	}
	if strings.Contains(notice, "\n") {
		t.Errorf("notice is more than one line: %q", notice)
	}
}

// Past the cutoff the old session is left alone, and the notice names it and
// how to get it back.
func TestVetAutoResumeStartsFreshPastTheCutoff(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd, now := "/work/proj", time.Now()
	writeSession(t, cwd, "old", now.Add(-72*time.Hour), userMsg("hi"), assistantMsg("hello"))

	id, notice := vetAutoResume(cwd, "old", 24*time.Hour, now)
	if id != "" {
		t.Fatalf("id = %q, want a fresh session", id)
	}
	for _, want := range []string{"old", "3d ago", "starting fresh", "--continue", "-r old", "(1d)"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q lacks %q", notice, want)
		}
	}
}

// autoResumeMaxAge = "0" turns the cutoff off.
func TestVetAutoResumeNoCutoff(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd, now := "/work/proj", time.Now()
	writeSession(t, cwd, "ancient", now.Add(-90*24*time.Hour), userMsg("hi"))

	if id, notice := vetAutoResume(cwd, "ancient", 0, now); id != "ancient" ||
		!strings.Contains(notice, "1 message ·") || !strings.Contains(notice, "90d ago") {
		t.Fatalf("vetAutoResume = %q, %q; want ancient resumed, 1 message, 90d ago", id, notice)
	}
}

// A recent session that ended in a refusal still starts fresh.
func TestVetAutoResumeSkipsARefusal(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd, now := "/work/proj", time.Now()
	writeSession(t, cwd, "refused", now.Add(-time.Minute), userMsg("hi"),
		session.Entry{Type: "assistant", Message: []byte(`{"role":"assistant","content":[]}`)})

	id, notice := vetAutoResume(cwd, "refused", 24*time.Hour, now)
	if id != "" || !strings.Contains(notice, "refusal") || !strings.Contains(notice, "--resume refused") {
		t.Fatalf("vetAutoResume = %q, %q; want a fresh start naming the refusal", id, notice)
	}
}

func TestConfigDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour:     "1d",
		7 * 24 * time.Hour: "7d",
		12 * time.Hour:     "12h",
		90 * time.Minute:   "1h30m",
		10 * time.Minute:   "10m",
		45 * time.Second:   "45s",
	} {
		if got := configDuration(d); got != want {
			t.Errorf("configDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestHumanAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now",
		5 * time.Minute:  "5m ago",
		26 * time.Hour:   "1d ago",
		3 * time.Hour:    "3h ago",
	} {
		if got := humanAgo(d); got != want {
			t.Errorf("humanAgo(%s) = %q, want %q", d, got, want)
		}
	}
}
