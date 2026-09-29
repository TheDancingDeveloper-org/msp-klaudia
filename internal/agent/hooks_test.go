package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/hooks"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// assistantToolUse builds an assistant turn that calls the Marker tool once.
func assistantMarkerCall(t *testing.T) anthropic.BetaMessage {
	t.Helper()
	const j = `{"role":"assistant","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"tu_1","name":"Marker","input":{}}]}`
	var m anthropic.BetaMessage
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// assistantText builds a tool-less assistant turn carrying text.
func assistantText(t *testing.T, text string) anthropic.BetaMessage {
	t.Helper()
	j := fmt.Sprintf(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":%q}]}`, text)
	var m anthropic.BetaMessage
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// TestPreToolUseBlockPreventsExecution: a PreToolUse hook that blocks must stop
// the tool from running and feed its reason back as an error tool_result.
func TestPreToolUseBlockPreventsExecution(t *testing.T) {
	rec := &captureRecorder{}
	provider := &scriptedProvider{turns: []anthropic.BetaMessage{assistantMarkerCall(t)}}
	loop := New(provider, tools.NewRegistry(markerTool{rec: rec}))

	runner := hooks.New(hooks.Config{
		PreToolUse: []hooks.Group{{Hooks: []hooks.Hook{{
			Command: `echo '{"decision":"block","reason":"denied by test"}'`,
		}}}},
	}, "")
	if runner == nil {
		t.Fatal("hooks.New returned nil")
	}

	res, err := loop.Run(context.Background(), Options{Hooks: runner, MaxTurns: 3}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The marker tool must never have executed.
	for _, row := range rec.snapshot() {
		if row == "DISPATCH" {
			t.Fatal("Marker executed despite PreToolUse block")
		}
	}

	// The block reason must reach the model as the tool_result content.
	if !messagesContain(res.Messages, "denied by test") {
		t.Error("block reason not found in tool_result")
	}
}

// TestStopBlockReentersLoopOnce: a Stop hook that blocks the first tool-less
// answer must re-enter the loop (injecting its reason), then allow the second
// answer through.
func TestStopBlockReentersLoopOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STOP_STATE", dir+"/n")

	provider := &scriptedProvider{turns: []anthropic.BetaMessage{
		assistantText(t, "first answer"),
		assistantText(t, "final answer"),
	}}
	loop := New(provider, tools.NewRegistry())

	// Blocks on its first invocation only (tracked via a counter file), then
	// permits stopping — so the loop re-enters exactly once.
	runner := hooks.New(hooks.Config{
		Stop: []hooks.Group{{Hooks: []hooks.Hook{{
			Command: `f="$STOP_STATE"; n=$(cat "$f" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$f"; ` +
				`if [ "$n" = "1" ]; then echo '{"decision":"block","reason":"keep working"}'; fi`,
		}}}},
	}, "")

	res, err := loop.Run(context.Background(), Options{Hooks: runner, MaxTurns: 10}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if provider.n != 2 {
		t.Errorf("provider consumed %d turns, want 2 (one re-entry)", provider.n)
	}
	if res.Text != "final answer" {
		t.Errorf("final text = %q, want %q", res.Text, "final answer")
	}
	if !messagesContain(res.Messages, "keep working") {
		t.Error("injected Stop reason not found in messages")
	}
}
