package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/greenthread-ai/klaudia/internal/config"
	"github.com/greenthread-ai/klaudia/internal/session"
)

// seedTitledSession writes a transcript whose first user message carries text,
// so a derived title has something to work from.
func seedTitledSession(t *testing.T, cwd, id, prompt string) {
	t.Helper()
	w, err := session.NewWriter(cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(`{"role":"user","content":[{"type":"text","text":` + jsonString(prompt) + `}]}`)
	if err := w.Append(session.Entry{Type: "user", SessionID: id, CWD: cwd, Message: msg}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func runSessions(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newSessionsCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSessionsLsListsSessions(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	seedTitledSession(t, cwd, "abc123", "Investigate the flaky test")

	out, err := runSessions(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "abc123") || !strings.Contains(out, "Investigate the flaky test") {
		t.Fatalf("ls output missing id/title:\n%s", out)
	}
	if !strings.Contains(out, cwd) {
		t.Fatalf("ls output missing project:\n%s", out)
	}
}

func TestSessionsLsEmpty(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	out, err := runSessions(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No saved sessions") {
		t.Fatalf("expected empty notice, got:\n%s", out)
	}
}

func TestSessionsLsJSON(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	seedTitledSession(t, cwd, "abc123", "Investigate the flaky test")

	out, err := runSessions(t, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var infos []session.SessionInfo
	if err := json.Unmarshal([]byte(out), &infos); err != nil {
		t.Fatalf("ls --json not valid JSON: %v\n%s", err, out)
	}
	if len(infos) != 1 || infos[0].ID != "abc123" || infos[0].Title != "Investigate the flaky test" {
		t.Fatalf("ls --json = %+v, want one titled session", infos)
	}
}

func TestSessionsRmDeletesSession(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/proj"
	seedTitledSession(t, cwd, "abc123", "task")

	out, err := runSessions(t, "rm", "abc123")
	if err != nil {
		t.Fatalf("rm failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Deleted session abc123") {
		t.Fatalf("rm output = %q, want deletion notice", out)
	}
	if _, err := os.Stat(session.Path(cwd, "abc123")); !os.IsNotExist(err) {
		t.Fatalf("transcript should be gone, stat err = %v", err)
	}
}

func TestSessionsRmMissingSessionErrors(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	if _, err := runSessions(t, "rm", "nope"); err == nil {
		t.Fatal("rm of a missing session should return a non-nil error")
	}
}

func TestSessionsRmInvalidIDIsUsageError(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	_, err := runSessions(t, "rm", "../escape")
	if err == nil {
		t.Fatal("rm of an invalid id should error")
	}
	if exitCodeFor(err) != ExitUsage {
		t.Fatalf("invalid id exit code = %d, want %d (usage)", exitCodeFor(err), ExitUsage)
	}
}

func TestSessionRetentionDefaultsAndDisable(t *testing.T) {
	def := sessionRetention(config.Sessions{})
	if def.MaxCount != defaultRetentionMax {
		t.Fatalf("default MaxCount = %d, want %d", def.MaxCount, defaultRetentionMax)
	}
	if def.MaxAge == 0 {
		t.Fatal("default MaxAge should be non-zero")
	}
	off := sessionRetention(config.Sessions{RetentionDays: -1, RetentionMax: -1})
	if off.MaxAge != 0 || off.MaxCount != 0 {
		t.Fatalf("negative config should disable caps, got %+v", off)
	}
	custom := sessionRetention(config.Sessions{RetentionDays: 7, RetentionMax: 5})
	if custom.MaxCount != 5 || custom.MaxAge != 7*24*time.Hour {
		t.Fatalf("custom retention = %+v, want 5 / 7d", custom)
	}
}
