package agent

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestContinueFromAppendsOneTurn(t *testing.T) {
	msgs := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("do the thing")),
		{Role: "assistant"},
	}
	out, ok := ContinueFrom(msgs)
	if !ok {
		t.Fatal("an assistant turn was not continuable")
	}
	if len(out) != 3 || out[2].Role != "user" {
		t.Fatalf("got %d messages, last role %q", len(out), out[len(out)-1].Role)
	}
	if _, ok := ContinueFrom(msgs[:1]); ok {
		t.Error("a conversation with no assistant turn was continued")
	}
}
