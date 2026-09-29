package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// modelRecordingProvider captures the model (and instruction) each request
// carried, and replies with a fixed assistant summary so Compact returns text.
type modelRecordingProvider struct {
	gotModel anthropic.Model
	gotLast  string // the final user message text (the summary instruction)
	reply    anthropic.BetaMessage
}

func (p *modelRecordingProvider) StreamTurn(_ context.Context, params anthropic.BetaMessageNewParams, _ api.StreamSink) (anthropic.BetaMessage, error) {
	p.gotModel = params.Model
	if n := len(params.Messages); n > 0 {
		last := params.Messages[n-1]
		if len(last.Content) > 0 && last.Content[0].OfText != nil {
			p.gotLast = last.Content[0].OfText.Text
		}
	}
	return p.reply, nil
}

func assistantText(t *testing.T, text string) anthropic.BetaMessage {
	t.Helper()
	// mustJSON (integration_test.go) encodes the string to a quoted JSON literal.
	raw := `{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":` + string(mustJSON(t, text)) + `}]}`
	var m anthropic.BetaMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal assistant: %v", err)
	}
	return m
}

// Compact must summarize with the model it is handed (the TUI passes the live
// session model), not a default baked in elsewhere.
func TestCompactUsesGivenModel(t *testing.T) {
	provider := &modelRecordingProvider{reply: assistantText(t, "the summary")}
	loop := New(provider, tools.NewRegistry())

	msgs := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("earlier conversation")),
	}
	_, summary, err := loop.Compact(context.Background(), msgs, "claude-haiku-4-5", "the auth flow")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if summary != "the summary" {
		t.Errorf("summary = %q, want %q", summary, "the summary")
	}
	if provider.gotModel != "claude-haiku-4-5" {
		t.Errorf("provider saw model %q, want claude-haiku-4-5", provider.gotModel)
	}
	// The focus rode along into the summary instruction.
	if !strings.Contains(provider.gotLast, "the auth flow") {
		t.Errorf("focus not threaded into the summary request: %q", provider.gotLast)
	}
}
