package compaction

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

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
