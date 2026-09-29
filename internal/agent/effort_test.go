package agent

import (
	"context"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

func TestRunSendsConfiguredEffortAndThinking(t *testing.T) {
	p := &paramsProvider{}
	if _, err := New(p, tools.NewRegistry()).Run(context.Background(), Options{
		Prompt: "hi", Model: "claude-opus-4-8", Permission: bypassPerm(),
		Effort: "xhigh", Thinking: api.ThinkingAdaptive,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.sent) != 1 {
		t.Fatalf("%d requests", len(p.sent))
	}
	got := p.sent[0]
	if got.OutputConfig.Effort != "xhigh" {
		t.Errorf("effort = %q", got.OutputConfig.Effort)
	}
	if got.Thinking.OfAdaptive == nil {
		t.Error("adaptive thinking was not requested")
	}
}

func TestRunWithoutSettingsSendsNeither(t *testing.T) {
	p := &paramsProvider{}
	if _, err := New(p, tools.NewRegistry()).Run(context.Background(), Options{
		Prompt: "hi", Model: "claude-opus-5", Permission: bypassPerm(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	got := p.sent[0]
	if got.OutputConfig.Effort != "" || got.Thinking.OfAdaptive != nil || got.Thinking.OfDisabled != nil {
		t.Errorf("unset settings changed the request: effort=%q thinking=%+v", got.OutputConfig.Effort, got.Thinking)
	}
}
