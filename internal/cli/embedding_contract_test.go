package cli

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/streamjson"
)

// TestEmbeddingContract pins docs/embedding.md against a real embedded run:
// every line type and field the document (and --capabilities) calls stable is
// present, and nothing the result line carries is missing from the advertised
// list. A change that fails here is a contract change — bump
// streamjson.ProtocolVersion if it removes or redefines a field, and update the
// document either way.
func TestEmbeddingContract(t *testing.T) {
	fake := &fakeChat{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	dir := workdir(t, srv.URL)

	lines, err := embed(t, dir, []string{"--session-id", "contract-1"}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	caps := streamjson.GetCapabilities(nil, nil)

	byType := map[string][]map[string]any{}
	for _, l := range lines {
		ty, _ := l["type"].(string)
		byType[ty] = append(byType[ty], l)
	}

	init := lines[0]
	if init["type"] != "system" || init["subtype"] != "init" {
		t.Fatalf("first line = %v, want system/init", init)
	}
	for _, f := range caps.StreamJSON.InitFields {
		if f == "resumed_from" {
			continue // only on a resumed session
		}
		if _, ok := init[f]; !ok {
			t.Errorf("system/init lacks %q: %v", f, init)
		}
	}

	if len(byType["assistant"]) == 0 {
		t.Fatalf("no assistant envelope in %v", lines)
	}
	for _, f := range []string{"message", "session_id", "uuid", "parent_tool_use_id"} {
		if _, ok := byType["assistant"][0][f]; !ok {
			t.Errorf("assistant envelope lacks %q", f)
		}
	}
	if byType["assistant"][0]["session_id"] != "contract-1" {
		t.Errorf("assistant session_id = %v", byType["assistant"][0]["session_id"])
	}

	results := byType["result"]
	if len(results) != 1 {
		t.Fatalf("%d result lines, want 1", len(results))
	}
	res := results[0]
	var got []string
	for k := range res {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string{}, caps.StreamJSON.ResultFields...)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Errorf("result fields = %v\nadvertised      = %v", got, want)
	}
	usage, _ := res["usage"].(map[string]any)
	for _, f := range caps.StreamJSON.UsageFields {
		if _, ok := usage[f]; !ok {
			t.Errorf("result usage lacks %q: %v", f, usage)
		}
	}
	if res["session_id"] != "contract-1" || res["is_error"] != false || res["subtype"] != "success" {
		t.Errorf("result = %v", res)
	}
}

func TestCapabilitiesProbeNeedsNoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	cmd := NewRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--capabilities"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var caps streamjson.Capabilities
	if err := json.Unmarshal(out.Bytes(), &caps); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if caps.Name != "klaudia" || caps.StreamJSON.Protocol != streamjson.ProtocolVersion || len(caps.PermissionModes) == 0 {
		t.Errorf("caps = %+v", caps)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
