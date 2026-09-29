package cli

import "testing"

func TestMCPServerOf(t *testing.T) {
	for name, want := range map[string]string{"mcp__loki__query": "loki", "mcp__a__b__c": "a"} {
		if got, ok := mcpServerOf(name); !ok || got != want {
			t.Errorf("mcpServerOf(%q) = %q,%v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"Read", "mcp__noseparator"} {
		if _, ok := mcpServerOf(name); ok {
			t.Errorf("mcpServerOf(%q) matched", name)
		}
	}
}
