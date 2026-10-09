package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestMcpHelp pins the shape both MCP servers share. #299 adds `browser` under
// the same parent; once it has merged, this test is what catches a second
// `mcp` command being registered instead of a subcommand.
func TestMcpHelp(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"mcp", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "codex-subagent") {
		t.Fatalf("mcp --help does not list codex-subagent:\n%s", out)
	}
	if strings.Count(out, "\n  mcp ") > 0 {
		t.Fatalf("mcp is registered twice:\n%s", out)
	}
}

func TestCodexSubagentHelpMentionsPosture(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"mcp", "codex-subagent", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"--sandbox", "--root", "--max-depth", "--max-jobs", "--codex"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %s", want)
		}
	}
}
