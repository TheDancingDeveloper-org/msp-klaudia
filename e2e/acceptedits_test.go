package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// acceptEdits, as a launcher passes it: an edit in the project runs without
// asking, and a shell command is asked about. Headless has no one to answer,
// so the ask is refused and the command never runs.
func TestAcceptEditsWritesButAsksBeforeACommand(t *testing.T) {
	m := NewFakeModel(t,
		Use("Write", map[string]any{"file_path": "edit.txt", "content": "banana"}),
		Use("Bash", map[string]any{"command": "touch ran.txt"}),
		Say("done."),
	)
	e := NewEnv(t, m)
	r := e.Headless("write edit.txt then touch ran.txt", "--permission-mode", "acceptEdits")

	if got, _ := os.ReadFile(e.Path("edit.txt")); string(got) != "banana" {
		t.Errorf("edit.txt = %q, want banana: the edit should not have been asked about\n%s", got, r.dump())
	}
	if _, err := os.Stat(e.Path("ran.txt")); err == nil {
		t.Errorf("the command ran without anyone approving it\n%s", r.dump())
	}
	results := toolResults(r.Events())
	if len(results) != 2 || results[0]["is_error"] == true || results[1]["is_error"] != true {
		t.Errorf("want the Write to succeed and the Bash refused, got %v\n%s", results, r.dump())
	}
}

// An edit outside the project asks too, so headless refuses it.
func TestAcceptEditsAsksBeforeAnEditOutsideTheProject(t *testing.T) {
	e := NewEnv(t, nil)
	outside := filepath.Join(filepath.Dir(e.Dir), "outside.txt")
	e.Model = NewFakeModel(t,
		Use("Write", map[string]any{"file_path": outside, "content": "x"}),
		Say("done."),
	)
	r := e.Headless("write outside the project", "--permission-mode", "acceptEdits")
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("an edit outside the project ran without being approved\n%s", r.dump())
	}
	// Refused by acceptEdits, not by the host gate: the path is not part of
	// this machine's OS, and autonomous would have written it.
	results := toolResults(r.Events())
	if len(results) != 1 || !strings.Contains(resultText(results[0]), "outside the workspace") {
		t.Errorf("want the Write refused as outside the workspace, got %v\n%s", results, r.dump())
	}
}

// A driven session, as Vogt runs one: the edit goes through untouched, the
// command reaches the driver as a can_use_tool ask, and the command runs only
// once the driver allows it. The init line reports the real mode, not the
// autonomous it used to be an alias for.
func TestAcceptEditsDrivenSessionAsksForTheCommandOnly(t *testing.T) {
	m := NewFakeModel(t,
		Use("Write", map[string]any{"file_path": "edit.txt", "content": "banana"}),
		Use("Bash", map[string]any{"command": "touch ran.txt"}),
		Say("done."),
	)
	e := NewEnv(t, m)
	s := e.StartStream("--permission-mode", "acceptEdits")

	s.SendUser("edit then run")
	if mode, _ := s.Next("system")["permissionMode"].(string); mode != "acceptEdits" {
		t.Errorf("init permissionMode = %q, want acceptEdits", mode)
	}
	req := s.Next("control_request")
	body, _ := req["request"].(map[string]any)
	if body["subtype"] != "can_use_tool" || body["tool_name"] != "Bash" {
		t.Fatalf("first ask = %v, want can_use_tool for Bash (the Write must not ask)", req)
	}
	if got, _ := os.ReadFile(e.Path("edit.txt")); string(got) != "banana" {
		t.Errorf("edit.txt = %q before the ask was answered, want the edit already made", got)
	}
	if _, err := os.Stat(e.Path("ran.txt")); err == nil {
		t.Fatal("the command ran before the driver answered")
	}
	s.Allow(req)
	if res := s.Next("result"); res["result"] != "done." {
		t.Fatalf("result = %v", res)
	}
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(e.Path("ran.txt")); err != nil {
		t.Errorf("the allowed command did not run: %v", err)
	}
}

// The config key takes the mode too, as it does the others.
func TestAcceptEditsFromConfig(t *testing.T) {
	m := NewFakeModel(t, Say("ok."))
	e := NewEnv(t, m)
	cfg := filepath.Join(e.Home, ".klaudia", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[permissions]\nmode = \"acceptEdits\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := e.StartStream()
	s.SendUser("hi")
	if mode, _ := s.Next("system")["permissionMode"].(string); mode != "acceptEdits" {
		t.Errorf("init permissionMode = %q, want acceptEdits", mode)
	}
	s.Next("result")
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
