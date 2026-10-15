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

	// A compiled binary, not a shell script. refuseWrapper rejects every
	// executable that starts with "#!", and it must keep doing so: a script
	// can prepend the bypass flag, which does not conflict with -s, and the
	// child would run unsandboxed. The fixture has to be something the check
	// accepts so the test can reach the tool at all.
	fake := filepath.Join(t.TempDir(), "codex")
	fakeBuild := exec.Command("go", "build", "-o", fake, ".")
	fakeBuild.Dir = filepath.Join(repoRoot, "cmd", "klaudia", "testdata", "fakecodex")
	if out, err := fakeBuild.CombinedOutput(); err != nil {
		t.Fatalf("building fake codex: %v\n%s", err, out)
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

	// WI-1639: output_schema was published as an array of bytes because the
	// field was a json.RawMessage, which the SDK infers from the Go type. A
	// model cannot pass a JSON object through that. It must be an object.
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var inputSchema []byte
	for _, tool := range listed.Tools {
		if tool.Name == "spawn_isolated" {
			inputSchema, err = json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if inputSchema == nil {
		t.Fatal("spawn_isolated is not published")
	}
	var parsed struct {
		Properties map[string]struct {
			Type  string   `json:"type"`
			Types []string `json:"types"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(inputSchema, &parsed); err != nil {
		t.Fatalf("input schema %s: %v", inputSchema, err)
	}
	prop := parsed.Properties["output_schema"]
	if prop.Type == "array" || containsString(prop.Types, "array") {
		t.Fatalf("output_schema is an array, so a model cannot pass a JSON object: %s", inputSchema)
	}
	if prop.Type != "object" && !containsString(prop.Types, "object") {
		t.Fatalf("output_schema type = %q/%v, want object: %s", prop.Type, prop.Types, inputSchema)
	}

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

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
