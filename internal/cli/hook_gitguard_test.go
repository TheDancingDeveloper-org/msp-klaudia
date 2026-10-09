package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/gitguard"
)

func TestHookGitguardRefusesDiscardingCommands(t *testing.T) {
	repo := hookGitRepo(t)
	writeFile(t, filepath.Join(repo, "keep.txt"), "user\n")
	git(t, repo, "add", "keep.txt")
	git(t, repo, "commit", "-m", "base")
	writeFile(t, filepath.Join(repo, "keep.txt"), "user edit\n")
	writeFile(t, filepath.Join(repo, "scratch.txt"), "untracked\n")

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sid := "sess-1"

	if code, _, stderr := runHook(t, sessionStart(sid, repo)); code != 0 {
		t.Fatalf("startup exit %d: %s", code, stderr)
	}

	// resume must not recapture, so a later edit of our own stays committable.
	writeFile(t, filepath.Join(repo, "ours.go"), "package ours\n")
	if code, _, stderr := runHook(t, payload(sid, repo, "SessionStart", "resume", "", nil)); code != 0 {
		t.Fatalf("resume exit %d: %s", code, stderr)
	}

	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"checkout", "git checkout -- keep.txt", "discard"},
		{"reset", "git reset --hard", "discard"},
		{"stash", "git stash", "discard"},
		{"clean", "git clean -f", "discard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runHook(t, bashHook(sid, repo, tc.cmd, ""))
			if code != 2 {
				t.Fatalf("exit %d, want 2; stderr %s", code, stderr)
			}
			if !strings.Contains(strings.ToLower(stderr), tc.want) && !strings.Contains(stderr, "Refused") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}

	code, _, stderr := runHook(t, bashHook(sid, repo, "git add ours.go && git commit -m ours", ""))
	if code != 0 {
		t.Fatalf("own commit refused (%d): %s", code, stderr)
	}
}

func TestHookGitguardRefusesOtherDirtyRepo(t *testing.T) {
	repo := hookGitRepo(t)
	git(t, repo, "commit", "--allow-empty", "-m", "base")
	other := hookGitRepo(t)
	writeFile(t, filepath.Join(other, "theirs.txt"), "v1\n")
	git(t, other, "add", "theirs.txt")
	git(t, other, "commit", "-m", "base")
	writeFile(t, filepath.Join(other, "theirs.txt"), "dirty\n")

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sid := "sess-other"
	if code, _, stderr := runHook(t, sessionStart(sid, repo)); code != 0 {
		t.Fatalf("startup %d %s", code, stderr)
	}
	code, _, stderr := runHook(t, bashHook(sid, repo, "git -C "+other+" checkout -- theirs.txt", ""))
	if code != 2 || !strings.Contains(stderr, "Refused") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestHookGitguardFailClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	repo := hookGitRepo(t)

	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"bad json", "{"},
		{"no session", `{"hook_event_name":"PreToolUse","tool_name":"Bash"}`},
		{"bad event", `{"session_id":"s","hook_event_name":"Stop"}`},
		{"no baseline", bashHookJSON("missing", repo, "git status", "")},
		{"bad tool input", `{"session_id":"s","hook_event_name":"PreToolUse","cwd":"` + repo + `","tool_name":"Bash","tool_input":"nope"}`},
	}
	// The bad-tool-input case needs a baseline or it fails for the missing one.
	if err := os.MkdirAll(filepath.Join(home, "gitguard"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := gitguard.MarshalBaseline(&gitguard.Baseline{Root: repo})
	if err := os.WriteFile(filepath.Join(home, "gitguard", "s.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runHook(t, tc.in)
			if code != 2 {
				t.Fatalf("exit %d, want 2; stderr %q", code, stderr)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatal("empty stderr does not block in Codex")
			}
		})
	}

	// A corrupt baseline refuses rather than running the command.
	if err := os.WriteFile(filepath.Join(home, "gitguard", "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runHook(t, bashHookJSON("bad", repo, "git status", ""))
	if code != 2 || !strings.Contains(stderr, "not readable") {
		t.Fatalf("corrupt baseline: %d %q", code, stderr)
	}
}

func TestHookGitguardApplyPatchTouchesOtherRepo(t *testing.T) {
	repo := hookGitRepo(t)
	git(t, repo, "commit", "--allow-empty", "-m", "base")
	other := hookGitRepo(t)
	writeFile(t, filepath.Join(other, "theirs.txt"), "v1\n")
	git(t, other, "add", "theirs.txt")
	git(t, other, "commit", "-m", "base")
	writeFile(t, filepath.Join(other, "theirs.txt"), "dirty\n")

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sid := "sess-patch"
	if code, _, stderr := runHook(t, sessionStart(sid, repo)); code != 0 {
		t.Fatalf("startup %d %s", code, stderr)
	}
	patch := "*** Begin Patch\n*** Update File: " + filepath.Join(other, "new.go") + "\n+package new\n*** End Patch\n"
	code, _, stderr := runHook(t, toolHook(sid, repo, "apply_patch", map[string]string{"command": patch}))
	if code != 0 {
		t.Fatalf("update should be allowed, got %d %s", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(home, "gitguard", sid+".json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := gitguard.UnmarshalBaseline(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(bOthers(b)) == 0 {
		t.Fatal("apply_patch did not capture the other repo")
	}
	del := "*** Begin Patch\n*** Delete File: " + filepath.Join(other, "theirs.txt") + "\n*** End Patch\n"
	code, _, stderr = runHook(t, toolHook(sid, repo, "apply_patch", map[string]string{"command": del}))
	if code != 2 || !strings.Contains(stderr, "Refused") {
		t.Fatalf("delete of protected file: %d %q", code, stderr)
	}
}

func bOthers(b *gitguard.Baseline) map[string]*gitguard.Baseline {
	raw, err := gitguard.MarshalBaseline(b)
	if err != nil {
		return nil
	}
	back, err := gitguard.UnmarshalBaseline(raw)
	if err != nil {
		return nil
	}
	// others is unexported; the round trip is the exported view. Re-marshal
	// and look at the JSON instead.
	var p struct {
		Others map[string]json.RawMessage `json:"others"`
	}
	_ = json.Unmarshal(raw, &p)
	_ = back
	out := map[string]*gitguard.Baseline{}
	for k := range p.Others {
		out[k] = &gitguard.Baseline{}
	}
	return out
}

func runHook(t *testing.T, in string) (int, string, string) {
	t.Helper()
	cmd := NewRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader(in))
	cmd.SetArgs([]string{"hook", "gitguard"})
	err := cmd.Execute()
	code := exitCodeFor(err)
	return code, stdout.String(), stderr.String()
}

func sessionStart(id, cwd string) string {
	return payload(id, cwd, "SessionStart", "startup", "", nil)
}

func bashHook(id, cwd, command, workdir string) string {
	in := map[string]string{"command": command}
	if workdir != "" {
		in["workdir"] = workdir
	}
	return toolHook(id, cwd, "Bash", in)
}

func bashHookJSON(id, cwd, command, workdir string) string {
	return bashHook(id, cwd, command, workdir)
}

func toolHook(id, cwd, tool string, input any) string {
	return payload(id, cwd, "PreToolUse", "", tool, input)
}

func payload(id, cwd, event, source, tool string, input any) string {
	p := map[string]any{
		"session_id":      id,
		"cwd":             cwd,
		"hook_event_name": event,
	}
	if source != "" {
		p["source"] = source
	}
	if tool != "" {
		p["tool_name"] = tool
		p["tool_input"] = input
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func hookGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "t")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
