package streamjson

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// A user message's text is taken from string content or from its text blocks;
// an image block without a base64 source yields no prompt and no image.
func TestDecodeUserContent(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"string", `{"role":"user","content":"hello"}`, "hello"},
		{"text blocks joined", `{"content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}`, "ab"},
		{"no text blocks", `{"content":[{"type":"image"}]}`, ""},
		{"content of another shape", `{"content":42}`, ""},
		{"not an object", `"just a string"`, ""},
		{"missing", ``, ""},
	} {
		got, imgs := decodeUserContent(json.RawMessage(tc.raw))
		if got != tc.want || len(imgs) != 0 {
			t.Errorf("%s: got %q (%d images), want %q and no images", tc.name, got, len(imgs), tc.want)
		}
	}
}

// A base64 image block rides along with the text; an image-only message is a
// prompt in its own right. A URL source or an unsupported media type is dropped.
func TestDecodeUserContentImages(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"see"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}},{"type":"image","source":{"type":"url","url":"https://x/a.png"}},{"type":"image","source":{"type":"base64","media_type":"image/bmp","data":"Qk0"}}]}`
	got, imgs := decodeUserContent(json.RawMessage(raw))
	if got != "see" || len(imgs) != 1 || imgs[0].MediaType != "image/png" || imgs[0].Base64 != "iVBOR" {
		t.Fatalf("got %q %+v, want text %q and one png", got, imgs, "see")
	}
	_, only := decodeUserContent(json.RawMessage(`{"content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"zzz"}}]}`))
	if len(only) != 1 || only[0].MediaType != "image/jpeg" {
		t.Fatalf("image-only: %+v", only)
	}
}

// An image-only user message starts a turn, and its image reaches the loop.
func TestRunImageOnlyMessage(t *testing.T) {
	input := `{"type":"user","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}}` + "\n"
	var got []tools.ResultImage
	runFn := func(_ context.Context, turn agent.Turn) (agent.Result, error) {
		if turn.Prompt != "" {
			t.Errorf("prompt = %q, want empty", turn.Prompt)
		}
		got = turn.Images
		return agent.Result{}, nil
	}
	if err := NewDriver(&lineSink{}).Run(context.Background(), strings.NewReader(input), runFn); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MediaType != "image/png" || got[0].Base64 != "iVBOR" {
		t.Fatalf("images = %+v", got)
	}
}

// Only user lines with text start a turn; history returned by one turn seeds
// the next; a failed turn is reported on its result line and does not wipe
// the history.
func TestRunSkipsNonPromptsAndCarriesHistory(t *testing.T) {
	input := strings.Join([]string{
		`not json`,
		`{"type":"keep_alive"}`,
		`{"type":"user","message":{"content":""}}`,
		`{"type":"user","message":{"content":"one"}}`,
		`{"type":"user","message":{"content":"two"}}`,
		`{"type":"user","message":{"content":"three"}}`,
	}, "\n") + "\n"

	var prompts []string
	var historyLens []int
	runFn := func(_ context.Context, turn agent.Turn) (agent.Result, error) {
		prompt, history := turn.Prompt, turn.History
		prompts = append(prompts, prompt)
		historyLens = append(historyLens, len(history))
		if prompt == "two" {
			return agent.Result{}, errors.New("upstream exploded")
		}
		msgs := append(append([]anthropic.BetaMessageParam(nil), history...),
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt)))
		return agent.Result{Text: "re:" + prompt, Messages: msgs}, nil
	}

	out := &lineSink{}
	if err := NewDriver(out).Run(context.Background(), strings.NewReader(input), runFn); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(prompts, ","); got != "one,two,three" {
		t.Fatalf("turns = %s, want one,two,three", got)
	}
	// The failed turn returned no Messages, so "three" still sees "one".
	if historyLens[0] != 0 || historyLens[1] != 1 || historyLens[2] != 1 {
		t.Errorf("history lengths = %v, want [0 1 1]", historyLens)
	}

	var results []map[string]any
	for _, l := range out.snapshot() {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["type"] == "result" {
			results = append(results, m)
		}
	}
	if len(results) != 3 {
		t.Fatalf("%d result lines, want 3", len(results))
	}
	if results[1]["is_error"] != true || results[1]["subtype"] != "error_during_execution" ||
		!strings.Contains(results[1]["result"].(string), "upstream exploded") {
		t.Errorf("failed turn result = %v", results[1])
	}
	if results[2]["result"] != "re:three" || results[2]["is_error"] != false {
		t.Errorf("recovered turn result = %v", results[2])
	}
}

// The peer's deny reason is passed through; an error response, or a success
// without a payload, is a plain deny.
func TestControlResponseDecisions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		want     permission.Decision
	}{
		{"deny with reason", `{"subtype":"success","request_id":"%s","response":{"behavior":"deny","message":"not today"}}`,
			permission.Decision{Behavior: permission.Deny, Message: "not today"}},
		{"error subtype", `{"subtype":"error","request_id":"%s","error":"client broke"}`,
			permission.Decision{Behavior: permission.Deny, Message: "denied"}},
		{"success without payload", `{"subtype":"success","request_id":"%s"}`,
			permission.Decision{Behavior: permission.Deny, Message: "denied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &lineSink{}
			d := NewDriver(out)
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()

			got := make(chan permission.Decision, 1)
			runFn := func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
				ap := turn.Approver
				got <- ap.Approve(ctx, agent.ApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{}`)})
				return agent.Result{}, nil
			}
			done := make(chan error, 1)
			go func() { done <- d.Run(context.Background(), pr, runFn) }()

			if _, err := io.WriteString(pw, `{"type":"user","message":{"content":"go"}}`+"\n"); err != nil {
				t.Fatal(err)
			}
			id := waitForRequestID(out)
			if id == "" {
				t.Fatal("no control_request")
			}
			if _, err := io.WriteString(pw, `{"type":"control_response","response":`+strings.Replace(tc.response, "%s", id, 1)+"}\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case dec := <-got:
				if dec != tc.want {
					t.Errorf("decision = %+v, want %+v", dec, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the approver never returned")
			}
			_ = pw.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Cancelling the context ends Run with the context's error, and a pending ask
// is denied as cancelled rather than left waiting.
func TestRunCancelledMidAsk(t *testing.T) {
	out := &lineSink{}
	d := NewDriver(out)
	d.AskTimeout = 0 // wait until the context ends
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan permission.Decision, 1)
	runFn := func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
		ap := turn.Approver
		got <- ap.Approve(ctx, agent.ApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{}`)})
		return agent.Result{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, pr, runFn) }()

	if _, err := io.WriteString(pw, `{"type":"user","message":{"content":"go"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if waitForRequestID(out) == "" {
		t.Fatal("no control_request")
	}
	cancel()
	select {
	case dec := <-got:
		if dec.Behavior != permission.Deny || dec.Message != "cancelled" {
			t.Errorf("decision = %+v, want deny/cancelled", dec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled ask kept waiting")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
