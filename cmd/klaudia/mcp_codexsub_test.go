package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/greenthread-ai/klaudia/internal/codexsub"
)

// TestCodexSubagentMCP drives `klaudia mcp codex-subagent` as a subprocess, the
// way Codex will, and checks the tool both spawns and adopts the result.
func TestCodexSubagentMCP(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "klaudia")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Join(repoRoot, "cmd", "klaudia")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building klaudia: %v\n%s", err, out)
	}

	fake := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\n" + `cd=""
while [ $# -gt 0 ]; do
  case "$1" in
    --cd) cd="$2"; shift 2;;
    *) shift;;
  esac
done
printf 'child was here\n' > "$cd/child.txt"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "--quiet", "-m", "first")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.CommandTransport{
		Command: exec.Command(bin, "mcp", "codex-subagent", "--codex", fake, "--root", root),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "spawn_isolated",
		Arguments: codexsub.Request{Task: "do it", WorkingDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	body, _ := json.Marshal(res.StructuredContent)
	var got codexsub.Result
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("structured content %s: %v", body, err)
	}
	if got.Final != "done" || len(got.Adopted) != 1 {
		t.Fatalf("result = %+v", got)
	}
	if b, err := os.ReadFile(filepath.Join(root, "child.txt")); err != nil || string(b) != "child was here\n" {
		t.Fatalf("adopted file = %q, %v", b, err)
	}
}
