package compaction

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// The summary request is sent without the conversation's system prompt and
// tools. On a model with preserved thinking a thinking block replayed there no
// longer matches the conversation it was minted in and the request is refused,
// so the summary request must carry none — while the live history keeps them.
func TestBuildSummaryRequestLeavesThinkingOut(t *testing.T) {
	history := []anthropic.BetaMessageParam{
		userText("fix the bug"),
		{
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				anthropic.NewBetaThinkingBlock("sig-1", ""),
				anthropic.NewBetaToolUseBlock("t1", map[string]any{"path": "a.go"}, "Read"),
			},
		},
		toolResult("t1", "package a"),
		{
			// A turn cut off while it was still thinking holds nothing else.
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				anthropic.NewBetaRedactedThinkingBlock("opaque"),
			},
		},
	}

	req := BuildSummaryRequest(history, "claude-fable-5-1", 4096, "")

	if len(req.Messages) != len(history)+1 {
		t.Fatalf("summary request has %d messages, want %d (history + instruction)", len(req.Messages), len(history)+1)
	}
	for i, m := range req.Messages {
		if len(m.Content) == 0 {
			t.Errorf("messages[%d] is empty; the API rejects that", i)
		}
		for j, b := range m.Content {
			if b.OfThinking != nil || b.OfRedactedThinking != nil {
				t.Errorf("messages[%d].content[%d] is a thinking block; the summary request must carry none", i, j)
			}
		}
	}
	if tu := req.Messages[1].Content[0].OfToolUse; tu == nil || tu.ID != "t1" {
		t.Errorf("the tool call must survive the strip, got %+v", req.Messages[1].Content)
	}
	if txt := req.Messages[3].Content[0].OfText; txt == nil || txt.Text != thinkingOmittedPlaceholder {
		t.Errorf("a thinking-only turn should carry the placeholder, got %+v", req.Messages[3].Content)
	}
	if txt := req.Messages[4].Content[0].OfText; txt == nil || txt.Text != SummaryInstruction {
		t.Errorf("the last message should be the summary instruction, got %+v", req.Messages[4].Content)
	}

	// The live history is untouched: its thinking stays for the next turn.
	if history[1].Content[0].OfThinking == nil || history[3].Content[0].OfRedactedThinking == nil {
		t.Error("BuildSummaryRequest mutated the conversation it was given")
	}
}

// finalUserText returns the text of the instruction message BuildSummaryRequest
// appends to the conversation (always the last message, a single text block).
func finalUserText(t *testing.T, req anthropic.BetaMessageNewParams) string {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatal("request carried no messages")
	}
	last := req.Messages[len(req.Messages)-1]
	if len(last.Content) == 0 || last.Content[0].OfText == nil {
		t.Fatal("final message is not a text block")
	}
	return last.Content[0].OfText.Text
}

func TestBuildSummaryRequestNoFocusUnchanged(t *testing.T) {
	msgs := []anthropic.BetaMessageParam{userText("do the thing")}
	req := BuildSummaryRequest(msgs, "claude-x", 4096, "")

	// The appended instruction must be byte-for-byte the original prompt, so a
	// plain /compact (and autocompact) behaves exactly as it did before focus.
	if got := finalUserText(t, req); got != SummaryInstruction {
		t.Errorf("no-focus instruction changed:\n got: %q\nwant: %q", got, SummaryInstruction)
	}
	// The original conversation is preserved ahead of the instruction.
	if len(req.Messages) != len(msgs)+1 {
		t.Errorf("message count = %d, want %d (conversation + instruction)", len(req.Messages), len(msgs)+1)
	}
	if req.Model != "claude-x" {
		t.Errorf("model = %q, want claude-x", req.Model)
	}
	if req.MaxTokens != 4096 {
		t.Errorf("maxTokens = %d, want 4096", req.MaxTokens)
	}
}

func TestBuildSummaryRequestWhitespaceFocusUnchanged(t *testing.T) {
	req := BuildSummaryRequest([]anthropic.BetaMessageParam{userText("x")}, "m", 10, "   \n\t ")
	if got := finalUserText(t, req); got != SummaryInstruction {
		t.Errorf("whitespace-only focus should be treated as no focus, got:\n%q", got)
	}
}

func TestBuildSummaryRequestFocusAppears(t *testing.T) {
	const focus = "the token-refresh race in auth.go"
	req := BuildSummaryRequest([]anthropic.BetaMessageParam{userText("x")}, "m", 10, focus)
	got := finalUserText(t, req)

	// The base instruction is still present...
	if !strings.Contains(got, SummaryInstruction) {
		t.Errorf("focused instruction dropped the base prompt:\n%q", got)
	}
	// ...and the user's focus text is carried into it verbatim.
	if !strings.Contains(got, focus) {
		t.Errorf("focus text %q not present in instruction:\n%q", focus, got)
	}
	// The focus is emphasized after the base prompt, not before it.
	if strings.Index(got, focus) < strings.Index(got, SummaryInstruction) {
		t.Error("focus should be appended after the base instruction, not ahead of it")
	}
}

func TestBuildSummaryRequestUsesGivenModel(t *testing.T) {
	// The whole point of threading the model: the request must summarize with
	// whatever model the caller passed (the live session model), never a default.
	for _, model := range []anthropic.Model{"claude-haiku-4-5", "claude-opus-5"} {
		req := BuildSummaryRequest(nil, model, 4096, "")
		if req.Model != model {
			t.Errorf("model = %q, want %q", req.Model, model)
		}
	}
}
