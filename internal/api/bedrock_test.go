package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

const bedrockTestModel = "au.anthropic.claude-sonnet-4-5-20250929-v1:0"

// bedrockEnv isolates the AWS default chain: static test credentials, no shared
// config, no instance metadata, and the endpoint pointed at the test server.
func bedrockEnv(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/klaudia-test")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/klaudia-test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("KLAUDIA_MAX_RETRIES", "0")
	t.Setenv(BedrockEndpointEnv, endpoint)
}

// chunk encodes one Anthropic stream event as a Bedrock eventstream "chunk".
func chunk(t *testing.T, enc *eventstream.Encoder, w io.Writer, event map[string]any) {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(raw)})
	msg := eventstream.Message{Payload: payload}
	msg.Headers.Set(":message-type", eventstream.StringValue("event"))
	msg.Headers.Set(":event-type", eventstream.StringValue("chunk"))
	msg.Headers.Set(":content-type", eventstream.StringValue("application/json"))
	if err := enc.Encode(w, msg); err != nil {
		t.Fatal(err)
	}
}

func exception(t *testing.T, enc *eventstream.Encoder, w io.Writer, code, message string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"message": message})
	msg := eventstream.Message{Payload: payload}
	msg.Headers.Set(":message-type", eventstream.StringValue("exception"))
	msg.Headers.Set(":exception-type", eventstream.StringValue(code))
	msg.Headers.Set(":content-type", eventstream.StringValue("application/json"))
	if err := enc.Encode(w, msg); err != nil {
		t.Fatal(err)
	}
}

func bedrockParams() anthropic.BetaMessageNewParams {
	return anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(bedrockTestModel),
		MaxTokens: 256,
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
		},
		Betas: append(append([]anthropic.AnthropicBeta{}, DefaultBetas...), WebToolBetas...),
		Tools: []anthropic.BetaToolUnionParam{
			{OfTool: &anthropic.BetaToolParam{
				Name:        "Read",
				InputSchema: anthropic.BetaToolInputSchemaParam{Properties: map[string]any{}},
			}},
			{OfWebSearchTool20260318: &anthropic.BetaWebSearchTool20260318Param{}},
			{OfWebFetchTool20260318: &anthropic.BetaWebFetchTool20260318Param{}},
		},
	}
}

func TestBedrockStreamTurn(t *testing.T) {
	var got struct {
		path, rawPath, auth string
		body                map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.rawPath, got.auth = r.URL.Path, r.URL.RawPath, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		enc := eventstream.NewEncoder()
		var buf bytes.Buffer
		chunk(t, enc, &buf, map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": bedrockTestModel,
			"content": []any{}, "usage": map[string]any{"input_tokens": 11, "output_tokens": 1},
		}})
		chunk(t, enc, &buf, map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		chunk(t, enc, &buf, map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "hello from bedrock"}})
		chunk(t, enc, &buf, map[string]any{"type": "content_block_stop", "index": 0})
		chunk(t, enc, &buf, map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 7}})
		chunk(t, enc, &buf, map[string]any{"type": "message_stop"})
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	bedrockEnv(t, srv.URL)

	client, err := NewBedrock(context.Background(), "ap-southeast-2", []string{"context-management-2025-06-27"})
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	msg, err := client.StreamTurn(context.Background(), bedrockParams(), StreamSink{OnText: func(s string) { streamed.WriteString(s) }})
	if err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	if want := "/model/" + bedrockTestModel + "/invoke-with-response-stream"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if !strings.HasPrefix(got.auth, "AWS4-HMAC-SHA256 Credential=AKIDTEST/") ||
		!strings.Contains(got.auth, "/ap-southeast-2/bedrock/aws4_request") {
		t.Errorf("request not SigV4-signed for bedrock in ap-southeast-2: %q", got.auth)
	}
	if got.body["anthropic_version"] != "bedrock-2023-05-31" {
		t.Errorf("anthropic_version = %v", got.body["anthropic_version"])
	}
	if _, ok := got.body["model"]; ok {
		t.Error("model must move into the URL, not stay in the body")
	}
	betas, _ := json.Marshal(got.body["anthropic_beta"])
	if string(betas) != `["context-management-2025-06-27"]` {
		t.Errorf("anthropic_beta = %s, want only the allow-listed flag", betas)
	}
	tools, _ := got.body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "Read" {
		t.Errorf("tools = %v, want only the client tool (server web tools stripped)", got.body["tools"])
	}

	if streamed.String() != "hello from bedrock" {
		t.Errorf("streamed text = %q", streamed.String())
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello from bedrock" {
		t.Errorf("assembled content = %+v", msg.Content)
	}
	if msg.Usage.InputTokens != 11 || msg.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want 11 in / 7 out", msg.Usage)
	}
}

func TestBedrockThrottlingIsTransient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		var buf bytes.Buffer
		exception(t, eventstream.NewEncoder(), &buf, "throttlingException", "Too many requests, please wait")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	bedrockEnv(t, srv.URL)

	client, err := NewBedrock(context.Background(), "ap-southeast-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.StreamTurn(context.Background(), bedrockParams(), StreamSink{})
	if err == nil {
		t.Fatal("want an error from a throttled stream")
	}
	if !IsTransient(err) {
		t.Errorf("throttling must be transient (retryable): %v", err)
	}
	if calls.Load() == 0 {
		t.Error("the request never reached the server")
	}
}

func TestBedrockNonTransientException(t *testing.T) {
	err := &testError{"received exception validationException: malformed input"}
	if IsBedrockTransient(err) || IsTransient(err) {
		t.Error("a validation exception must not be retried")
	}
	for _, code := range []string{"throttlingException", "ServiceUnavailableException", "modelStreamErrorException"} {
		if !IsBedrockTransient(&testError{"received exception " + code + ": x"}) {
			t.Errorf("%s should be transient", code)
		}
	}
}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }

func TestNewBedrockNeedsARegion(t *testing.T) {
	bedrockEnv(t, "")
	if _, err := NewBedrock(context.Background(), "", nil); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("want a region error, got %v", err)
	}
	t.Setenv("AWS_REGION", "ap-southeast-2")
	c, err := NewBedrock(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.endpoint() != "https://bedrock-runtime.ap-southeast-2.amazonaws.com" {
		t.Errorf("endpoint = %q", c.endpoint())
	}
}
