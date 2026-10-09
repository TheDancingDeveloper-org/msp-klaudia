package agent

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/subagent"
)

func TestSubagentModel(t *testing.T) {
	for _, tc := range []struct{ parent, own, want string }{
		{"claude-opus-5", "", "claude-opus-5"},
		{"claude-opus-5", "sonnet", "claude-sonnet-5"},
		{"claude-opus-5", "claude-haiku-4-5", "claude-haiku-4-5"},
		// A non-Claude session is not moved onto a Claude id it cannot serve.
		{"openai/gpt-5.5", "sonnet", "openai/gpt-5.5"},
		{"openai/gpt-5.5", "openai/gpt-5.5-mini", "openai/gpt-5.5-mini"},
	} {
		if got := string(subagentModel(anthropicModel(tc.parent), tc.own)); got != tc.want {
			t.Errorf("subagentModel(%q, %q) = %q, want %q", tc.parent, tc.own, got, tc.want)
		}
	}
}

func TestResolveChildModelPrecedenceAndNotice(t *testing.T) {
	parent := anthropic.Model("claude-opus-5")
	for _, tc := range []struct {
		name, provider, requested, typeModel, want, notice string
	}{
		{"tool input wins", "", "haiku", "sonnet", "claude-haiku-4-5", "claude-haiku-4-5"},
		{"type model otherwise", "", "", "sonnet", "claude-sonnet-5", "claude-sonnet-5"},
		{"parent when neither names one", "", "", "", "claude-opus-5", ""},
		{"alias on a non-claude provider is replaced", "openai", "sonnet", "", "claude-opus-5", "not served"},
		{"a full non-claude id is kept", "openai", "openai/gpt-5.5-mini", "", "openai/gpt-5.5-mini", "openai/gpt-5.5-mini"},
	} {
		s := &Spawner{model: parent, providerName: tc.provider}
		got, note := s.resolveChildModel(ChildSpec{RequestedModel: tc.requested}, subagent.Type{Model: tc.typeModel})
		if string(got) != tc.want || !strings.Contains(note, tc.notice) {
			t.Errorf("%s: resolveChildModel = %q, %q; want %q containing %q", tc.name, got, note, tc.want, tc.notice)
		}
	}
}

func TestChildMaxTurnsCapsAtTheSession(t *testing.T) {
	s := &Spawner{maxTurns: 10}
	if got := s.childMaxTurns(ChildSpec{RequestedMaxTurns: 4}, subagent.Type{MaxTurns: 20}); got != 4 {
		t.Errorf("tool input should win: got %d", got)
	}
	if got := s.childMaxTurns(ChildSpec{RequestedMaxTurns: 40}, subagent.Type{MaxTurns: 20}); got != 10 {
		t.Errorf("session bound should cap the tool input: got %d", got)
	}
	if got := s.childMaxTurns(ChildSpec{}, subagent.Type{MaxTurns: 7}); got != 7 {
		t.Errorf("type bound should apply when the call names none: got %d", got)
	}
	if got := (&Spawner{}).childMaxTurns(ChildSpec{}, subagent.Type{}); got != defaultSubagentMaxTurns {
		t.Errorf("no bound anywhere should fall back to the default: got %d", got)
	}
}

func anthropicModel(s string) anthropic.Model { return anthropic.Model(s) }
