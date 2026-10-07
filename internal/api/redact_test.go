package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactRequestDropsContentKeepsShape(t *testing.T) {
	body := []byte(`{
		"model": "grok",
		"messages": [
			{"role": "system", "content": "you are a helpful agent with a long secret prompt"},
			{"role": "user", "content": "read the credentials file"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "Read", "arguments": "{\"file_path\":\"/etc/secret\"}"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": [
				{"type": "text", "text": "password=hunter2"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
			]}
		],
		"tools": [{"type": "function", "function": {"name": "Read", "description": "reads a file", "parameters": {"type": "object"}}}]
	}`)

	var got map[string]any
	if err := json.Unmarshal(redactRequest(body), &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(sys, "[redacted ") || strings.Contains(sys, "secret") {
		t.Fatalf("system content = %q, want a length mark", sys)
	}
	call := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "Read" {
		t.Fatalf("tool name lost: %v", fn["name"])
	}
	if args := fn["arguments"].(string); strings.Contains(args, "secret") || !strings.HasPrefix(args, "[redacted ") {
		t.Fatalf("arguments = %q, want them redacted", args)
	}
	parts := msgs[3].(map[string]any)["content"].([]any)
	if strings.Contains(parts[0].(map[string]any)["text"].(string), "hunter2") {
		t.Fatal("tool result text leaked")
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if strings.Contains(img, "base64") {
		t.Fatalf("image url = %q, want it redacted", img)
	}
	tool := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tool["name"] != "Read" || tool["parameters"] == nil {
		t.Fatalf("tool schema shape lost: %v", tool)
	}
}

func TestDumpFailedRequestOffByDefault(t *testing.T) {
	t.Setenv("KLAUDIA_DUMP_FAILED_REQUEST", "")
	dumpFailedRequest(400, []byte(`{"messages":[{"role":"user","content":"secret"}]}`))
	// Nothing to assert except that it returns; the point is no panic and no file.
}

func TestDumpFailedRequestWritesRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dump.log")
	t.Setenv("KLAUDIA_DUMP_FAILED_REQUEST", path)
	dumpFailedRequest(400, []byte(`{"model":"m","messages":[{"role":"user","content":"secret prompt"}]}`))

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("dump leaked content: %s", b)
	}
	if !strings.Contains(string(b), `"status":400`) || !strings.Contains(string(b), "redacted") {
		t.Fatalf("dump = %s, want status and a redaction mark", b)
	}
}
