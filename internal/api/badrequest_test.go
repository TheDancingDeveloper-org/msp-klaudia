package api

import (
	"strings"
	"testing"
)

// The body api.theclawbay.com sends for an unknown model: "error" is a bare
// string, and the served models are listed alongside (#245).
const clawbayUnsupported = `{"error":"unsupported model: openai/grok-4.7","supportedModels":["gpt-5.5","grok-4.7","grok-4.6"]}`

func TestBadRequestQuotesTheProvider(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string
	}{
		{"error as a string", clawbayUnsupported, []string{"Bad request (400)", "unsupported model: openai/grok-4.7"}},
		{"top-level message", `{"message":"temperature is not supported"}`, []string{"temperature is not supported"}},
		{"FastAPI detail", `{"detail":"Field required: messages"}`, []string{"Field required: messages"}},
		{"plain text", "upstream said no\n\tbecause reasons", []string{"the provider returned: upstream said no because reasons"}},
		{"unknown JSON", `{"oops":true}`, []string{`the provider returned: {"oops":true}`}},
	}
	for _, c := range cases {
		got := FriendlyError(&OpenAIError{StatusCode: 400, Body: c.body})
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
		if strings.Contains(got, "rejected the request shape") {
			t.Errorf("%s: still the generic guess: %q", c.name, got)
		}
	}
	if got := FriendlyError(&OpenAIError{StatusCode: 400}); !strings.Contains(got, "rejected the request shape") {
		t.Errorf("an empty body keeps the generic message: %q", got)
	}
	long := strings.Repeat("x", 5000)
	if got := FriendlyError(&OpenAIError{StatusCode: 400, Body: long}); len(got) > maxBodyQuote+200 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long body was not truncated (%d bytes)", len(got))
	}
}

func TestBadRequestExplainsAPrefixedModelID(t *testing.T) {
	err := annotateNotFound(&OpenAIError{StatusCode: 400, Body: clawbayUnsupported}, "openai/grok-4.7", "https://api.theclawbay.com/v1")
	got := FriendlyError(err)
	for _, w := range []string{"unsupported model: openai/grok-4.7. The endpoint", `The endpoint serves "grok-4.7"`, `set model = "grok-4.7"`, `drop the "openai/" prefix`} {
		if !strings.Contains(got, w) {
			t.Errorf("%q lacks %q", got, w)
		}
	}

	// Without a supported list, the hint is conditional.
	err = annotateNotFound(&OpenAIError{StatusCode: 400, Body: `{"error":"bad model"}`}, "openai/gpt-5.5", "https://x/v1")
	if got := FriendlyError(err); !strings.Contains(got, "sent to the endpoint exactly as written") || !strings.Contains(got, `"gpt-5.5"`) {
		t.Errorf("no conditional prefix hint: %q", got)
	}

	// A bare id gets no hint; nor does an Anthropic error.
	err = annotateNotFound(&OpenAIError{StatusCode: 400, Body: `{"error":"bad"}`}, "grok-4.7", "https://x/v1")
	if got := FriendlyError(err); strings.Contains(got, "exactly as written") {
		t.Errorf("hint for a bare id: %q", got)
	}
}

func TestUnsupportedModelIsModelNotFound(t *testing.T) {
	if !IsModelNotFound(&OpenAIError{StatusCode: 400, Body: clawbayUnsupported}) {
		t.Error(`"unsupported model" is not recognised as an unknown model`)
	}
}
