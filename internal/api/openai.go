package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// OpenAIProvider talks to an OpenAI-compatible Chat Completions endpoint
// (e.g. GreenThread's GPT-5.5). It implements Provider by translating the
// canonical Anthropic request into the Chat Completions schema, streaming the
// SSE response, and assembling an anthropic.BetaMessage so the rest of Klaudia
// (loop, tools, sessions, compaction) is unchanged.
type OpenAIProvider struct {
	baseURL      string // e.g. https://api.demo.gthread.dev/v1
	apiKey       string
	extraHeaders map[string]string // resolved header -> value (e.g. CF Access service token)
	temperature  *float64
	http         *http.Client
	// sessionID labels log lines for this run so a log scraper can group them.
	// Empty when the caller has no session (tests, one-shot probes).
	sessionID string
}

// NewOpenAIProvider builds the provider. baseURL should include the /v1 suffix.
// temperature may be nil (omit from request) or a pointer to a value. extraHeaders are
// applied to every request (may be nil); use them for endpoints gated by non-bearer headers.
func NewOpenAIProvider(baseURL, apiKey string, temperature *float64, extraHeaders map[string]string) *OpenAIProvider {
	return &OpenAIProvider{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		extraHeaders: extraHeaders,
		temperature:  temperature,
		// Same client the Anthropic provider uses: HTTP/2 keepalive pings, so a
		// connection a NAT or sleep cycle killed is detected in seconds instead
		// of the request being written into a black hole. No overall Timeout —
		// a streamed turn legitimately runs for minutes; the idle watchdog
		// (streamIdleTimeout) bounds a silent stream instead.
		http: newHTTPClient(),
	}
}

// SetHTTPClient replaces the HTTP client. Tests inject a client pointed at a
// local server; production callers should leave the default (h2 keepalive).
func (p *OpenAIProvider) SetHTTPClient(c *http.Client) { p.http = c }

// SetSessionID labels this provider's log lines with the run's session id.
func (p *OpenAIProvider) SetSessionID(id string) { p.sessionID = id }

// setAuth sets Authorization (only when a key is configured — a header-authenticated endpoint
// has none) and applies every configured extra header. Used by streamAttempt and ListModels so
// the two request paths authenticate identically.
func (p *OpenAIProvider) setAuth(req *http.Request) {
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for name, value := range p.extraHeaders {
		req.Header.Set(name, value)
	}
}

// --- Anthropic wire shapes we read from the request params ---

type aMsg struct {
	Role    string   `json:"role"`
	Content []aBlock `json:"content"`
}

type aBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result (string or blocks)
	Source    json.RawMessage `json:"source,omitempty"`      // image
}

// --- OpenAI Chat Completions shapes ---

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
}

type oaContentPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

type oaImageURL struct {
	URL string `json:"url"`
}

type oaToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaRequest struct {
	Model               string      `json:"model"`
	Messages            []oaMessage `json:"messages"`
	Tools               []oaTool    `json:"tools,omitempty"`
	Temperature         *float64    `json:"temperature,omitempty"` // optional per OpenAI spec; omitted when nil
	MaxCompletionTokens int64       `json:"max_completion_tokens,omitempty"`
	// ReasoningEffort carries output_config.effort (see openAIReasoningEffort).
	// Omitted when no effort is configured: non-reasoning models reject it.
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Stream          bool          `json:"stream"`
	StreamOptions   *oaStreamOpts `json:"stream_options,omitempty"`
}

type oaStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
	} `json:"function"`
}

// StreamTurn implements Provider.
func (p *OpenAIProvider) StreamTurn(ctx context.Context, params anthropic.BetaMessageNewParams, sink StreamSink) (anthropic.BetaMessage, error) {
	reqBody, err := p.translateRequest(params)
	if err != nil {
		return anthropic.BetaMessage{}, err
	}
	body, _ := json.Marshal(reqBody)

	// Same idle-watchdog + stall-retry policy as the Anthropic provider: a
	// half-open SSE connection would otherwise park bufio.Scanner.Scan() forever
	// with the TUI stuck on "thinking…". See provider.go for the rationale.
	idle := streamIdleTimeout()
	for attempt := 0; ; attempt++ {
		acc, delivered, stalled, serr := p.streamAttempt(ctx, body, string(params.Model), sink, idle)
		if !stalled && errors.Is(serr, errIncomplete) && ctx.Err() == nil {
			if !delivered && attempt < maxStreamStallRetries {
				continue
			}
			if delivered {
				return acc, fmt.Errorf("%w (%w): %w", ErrStreamInterrupted, errStallMidStream, serr)
			}
			return acc, fmt.Errorf("%w: %w", ErrStreamInterrupted, serr)
		}
		if !stalled {
			return acc, annotateNotFound(serr, string(params.Model), p.baseURL)
		}
		if !delivered && attempt < maxStreamStallRetries {
			continue
		}
		return acc, fmt.Errorf("%w: no data for %s: %w", ErrStreamStalled, idle, serr)
	}
}

// streamAttempt issues one Chat Completions request and consumes its SSE
// response under an idle watchdog. It returns the assembled message, whether
// any output reached the sink, whether the watchdog tripped (a stall distinct
// from a user interrupt), and the terminal error. The watchdog cancels only a
// child of ctx, so a tripped watchdog with ctx still live marks a genuine
// stall; a cancelled ctx is the user's interrupt and is returned as-is.
func (p *OpenAIProvider) streamAttempt(ctx context.Context, body []byte, model string, sink StreamSink, idle time.Duration) (acc anthropic.BetaMessage, delivered, stalled bool, err error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return anthropic.BetaMessage{}, false, false, err
	}
	p.setAuth(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.doWithRetry(httpReq, body)
	if err != nil {
		return anthropic.BetaMessage{}, false, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		b, _ := readAll(resp.Body, 4096)
		dumpFailedRequest(resp.StatusCode, body)
		return anthropic.BetaMessage{}, false, false, &OpenAIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(b)}
	}

	var tripped atomic.Bool
	var timer *time.Timer
	if idle > 0 {
		timer = time.AfterFunc(idle, func() {
			tripped.Store(true)
			cancel() // aborts the in-flight resp.Body read → Scan() unblocks
		})
		defer timer.Stop()
	}
	onActivity := func() {
		if timer != nil {
			timer.Reset(idle)
		}
	}

	acc, delivered, err = p.consumeStream(resp.Body, model, sink, onActivity)
	if tripped.Load() && ctx.Err() == nil {
		return acc, delivered, true, context.DeadlineExceeded
	}
	return acc, delivered, false, err
}

// translateRequest converts the Anthropic params into an OpenAI chat request.
func (p *OpenAIProvider) translateRequest(params anthropic.BetaMessageNewParams) (oaRequest, error) {
	out := oaRequest{
		Model:               string(params.Model),
		Temperature:         p.temperature,
		MaxCompletionTokens: params.MaxTokens,
		ReasoningEffort:     openAIReasoningEffort(params.OutputConfig.Effort),
		Stream:              true,
		StreamOptions:       &oaStreamOpts{IncludeUsage: true},
	}

	// System prompt → a leading system message.
	var sys strings.Builder
	for _, blk := range params.System {
		sys.WriteString(blk.Text)
	}
	if sys.Len() > 0 {
		out.Messages = append(out.Messages, oaMessage{Role: "system", Content: sys.String()})
	}

	// Conversation messages.
	rawMsgs, _ := json.Marshal(params.Messages)
	var amsgs []aMsg
	if err := json.Unmarshal(rawMsgs, &amsgs); err != nil {
		return out, fmt.Errorf("translate messages: %w", err)
	}
	for _, m := range amsgs {
		out.Messages = append(out.Messages, translateMessage(m)...)
	}

	// Custom tools → function tools (skip server-side web tools, which have no
	// input_schema and no OpenAI equivalent).
	rawTools, _ := json.Marshal(params.Tools)
	var atools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	_ = json.Unmarshal(rawTools, &atools)
	for _, t := range atools {
		if t.Name == "" || len(t.InputSchema) == 0 {
			continue
		}
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		var schema any
		_ = json.Unmarshal(t.InputSchema, &schema)
		ot.Function.Parameters = schema
		out.Tools = append(out.Tools, ot)
	}
	return out, nil
}

// translateMessage converts one Anthropic message into one or more OpenAI
// messages (tool results become separate role:"tool" messages).
func translateMessage(m aMsg) []oaMessage {
	var text strings.Builder
	var toolCalls []oaToolCall
	var toolResults []oaMessage

	for _, b := range m.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			tc := oaToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name = b.Name
			tc.Function.Arguments = string(b.Input)
			if tc.Function.Arguments == "" {
				tc.Function.Arguments = "{}"
			}
			toolCalls = append(toolCalls, tc)
		case "tool_result":
			toolResults = append(toolResults, oaMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    toolResultContent(b.Content),
			})
		}
	}

	var msgs []oaMessage
	if m.Role == "assistant" {
		am := oaMessage{Role: "assistant", Content: text.String(), ToolCalls: toolCalls}
		msgs = append(msgs, am)
	} else { // user
		if text.Len() > 0 {
			msgs = append(msgs, oaMessage{Role: "user", Content: text.String()})
		}
		msgs = append(msgs, toolResults...)
	}
	return msgs
}

// toolResultContent extracts text and image blocks from an Anthropic tool_result
// content field. Image blocks become OpenAI image_url parts using data URLs.
func toolResultContent(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []aBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	parts := make([]oaContentPart, 0, len(blocks))
	var text strings.Builder
	hasImage := false
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		parts = append(parts, oaContentPart{Type: "text", Text: text.String()})
		text.Reset()
	}
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "image":
			flushText()
			if p, ok := imagePart(blk); ok {
				parts = append(parts, p)
				hasImage = true
			}
		}
	}
	flushText()
	if !hasImage {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return parts
}

func imagePart(blk aBlock) (oaContentPart, bool) {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(blk.Source, &source) != nil {
		return oaContentPart{}, false
	}
	url := source.URL
	if url == "" && source.Type == "base64" && source.MediaType != "" && source.Data != "" {
		url = "data:" + source.MediaType + ";base64," + source.Data
	}
	if url == "" {
		return oaContentPart{}, false
	}
	return oaContentPart{Type: "image_url", ImageURL: &oaImageURL{URL: url}}, true
}

func readAll(r interface{ Read([]byte) (int, error) }, max int) (string, error) {
	buf := make([]byte, max)
	n, _ := r.Read(buf)
	return string(buf[:n]), nil
}

// OpenAIError is a non-2xx response from the OpenAI-compatible endpoint. It
// carries the status so FriendlyError can classify it like an Anthropic error.
type OpenAIError struct {
	StatusCode int
	Body       string
}

func (e *OpenAIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("openai endpoint %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("openai endpoint %d", e.StatusCode)
}

// OpenAIErrorPayload is the structured error envelope OpenAI-compatible
// providers return inside the response body:
//
//	{"error": {"message": "...", "type": "...", "code": "..."}}
//
// Surfacing it lets FriendlyError pass through the provider's own (often
// actionable) wording instead of a generic guess.
type OpenAIErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	// Code is provider-dependent: most OpenAI-compatible APIs send a string
	// like "insufficient_quota", but some (e.g. gpt-oss-120b hosts) send a
	// numeric HTTP status. json.RawMessage accepts both shapes without
	// erroring the whole envelope parse.
	Code json.RawMessage `json:"code"`
}

// Payload parses the response body for that envelope. Returns nil when the
// body is empty or doesn't match (some providers ship plain-text errors).
//
// Besides the OpenAI shape it accepts the variants OpenAI-compatible hosts
// actually send: "error" as a bare string ({"error":"unsupported model: x"},
// seen from api.theclawbay.com — #245), and a top-level "message" or "detail"
// (FastAPI-style servers). Each becomes Message, so FriendlyError can quote it
// instead of guessing.
func (e *OpenAIError) Payload() *OpenAIErrorPayload {
	if e == nil || e.Body == "" {
		return nil
	}
	var wrap struct {
		Error   json.RawMessage `json:"error"`
		Message json.RawMessage `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal([]byte(e.Body), &wrap); err != nil {
		return nil
	}
	var p OpenAIErrorPayload
	if len(wrap.Error) > 0 && json.Unmarshal(wrap.Error, &p) == nil &&
		(p.Message != "" || p.Type != "" || len(p.Code) > 0) {
		return &p
	}
	for _, raw := range []json.RawMessage{wrap.Error, wrap.Message, wrap.Detail} {
		var str string
		if len(raw) > 0 && json.Unmarshal(raw, &str) == nil && strings.TrimSpace(str) != "" {
			return &OpenAIErrorPayload{Message: strings.TrimSpace(str)}
		}
	}
	return nil
}

// SupportedModels returns the model list some hosts attach to an unknown-model
// error ({"supportedModels":[...]} or {"supported_models":[...]}), or nil.
func (e *OpenAIError) SupportedModels() []string {
	if e == nil || e.Body == "" {
		return nil
	}
	var b struct {
		Camel []string `json:"supportedModels"`
		Snake []string `json:"supported_models"`
	}
	if json.Unmarshal([]byte(e.Body), &b) != nil {
		return nil
	}
	if len(b.Camel) > 0 {
		return b.Camel
	}
	return b.Snake
}

// opaque400Retries caps extra attempts spent on an opaque 400, beyond the
// ordinary retry budget. A 400 whose body names nothing actionable is usually a
// transient gateway glitch — seen live as a bare "invalid request" on a request
// the same endpoint then accepted unchanged — but one that persists is a real
// client error and must fail fast rather than loop. A 400 that names its
// problem is never retried.
const opaque400Retries = 2

// doWithRetry issues the request, retrying transient failures (connection
// errors and 429/5xx) with exponential backoff that honors Retry-After. An
// opaque 400 (empty body, or the bare phrase "invalid request", enveloped or not)
// gets up to opaque400Retries extra attempts; a 400 carrying a real message is
// returned at once. The request body is re-created from bodyBytes each attempt.
func (p *OpenAIProvider) doWithRetry(req *http.Request, bodyBytes []byte) (*http.Response, error) {
	max := maxRetries()
	opaqueLeft := opaque400Retries
	var lastErr error
	var wait time.Duration
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(wait):
			}
		}
		clone := req.Clone(req.Context())
		clone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		clone.ContentLength = int64(len(bodyBytes))

		wait = backoff(attempt + 1)
		start := time.Now()
		resp, err := p.http.Do(clone)
		if err != nil {
			lastErr = err
			logModelCall(modelCall{
				session: p.sessionID, host: req.URL.Host, model: modelOf(bodyBytes),
				attempt: attempt + 1, max: max + 1, latency: time.Since(start),
				err: err.Error(),
			})
			if attempt >= max {
				return nil, lastErr
			}
			continue
		}

		retry, why := retryStatus(resp, attempt, max, &opaqueLeft)
		logModelCall(modelCall{
			session: p.sessionID, host: req.URL.Host, model: modelOf(bodyBytes),
			attempt: attempt + 1, max: max + 1, status: resp.StatusCode,
			requestID: requestIDOf(resp), latency: time.Since(start),
			retry: retry, err: why,
		})
		if !retry {
			return resp, nil
		}
		if w, told := retryAfter(resp.Header.Get("Retry-After"), time.Now()); told {
			// A server asking for a long wait is answered now, not after
			// minutes of silence: its error says when to try again.
			if w > maxRetryAfter {
				return resp, nil
			}
			wait = w
		}
		_ = resp.Body.Close()
		lastErr = &OpenAIError{StatusCode: resp.StatusCode, Body: why}
	}
}

// maxRetryAfter is the longest Retry-After the provider waits out itself.
const maxRetryAfter = 60 * time.Second

// retryAfter parses a Retry-After value — delay-seconds or an HTTP date —
// into a wait from now. The comment on doWithRetry always said it honoured
// the header; it never read it, so five retries finished in about fifteen
// seconds against a server asking for a minute. told is false when the header
// is absent or unparseable.
func retryAfter(v string, now time.Time) (wait time.Duration, told bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// backoff returns an exponential delay for the given (1-based) attempt, capped
// at 8s. Tests shrink the unit via the unexported backoffUnit.
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * backoffUnit
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// backoffUnit is the base of the exponential backoff. Production leaves it at
// half a second; tests set it to zero so retry accounting can be checked
// without waiting.
var backoffUnit = 500 * time.Millisecond

// retryStatus decides whether a response is worth another attempt. 429 and 5xx
// retry within the ordinary budget; a 400 retries only while it is opaque and
// opaqueLeft remains. Anything else — including a 400 that names its cause —
// is final. why is a short, log-safe description and never contains request
// content.
func retryStatus(resp *http.Response, attempt, max int, opaqueLeft *int) (bool, string) {
	switch {
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		if attempt >= max {
			return false, ""
		}
		return true, ""
	case resp.StatusCode == 400:
		detail := errorDetail(resp)
		if detail != "" || *opaqueLeft <= 0 {
			return false, detail
		}
		*opaqueLeft--
		return true, "opaque 400"
	default:
		return false, ""
	}
}

// errorDetail reports the actionable part of an error body: the parsed error
// envelope's message, or else the raw text, unless all it says is the bare
// phrase "invalid request". An empty result means nothing actionable — the opaque
// case. Only the first 512 bytes are consumed, and the body is reassembled so
// the caller can still read the whole response.
func errorDetail(resp *http.Response) string {
	raw, _ := readAll(resp.Body, 512)
	resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(raw), resp.Body))
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	if p := (&OpenAIError{StatusCode: resp.StatusCode, Body: text}).Payload(); p != nil && strings.TrimSpace(p.Message) != "" {
		text = strings.TrimSpace(p.Message)
	}
	// The bare phrase says nothing whether or not it arrives in an envelope.
	if strings.EqualFold(strings.TrimRight(text, "."), "invalid request") {
		return ""
	}
	return truncate(text, 200)
}

// modelOf pulls the "model" field from a marshalled request for logging,
// without decoding the whole body.
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

func requestIDOf(resp *http.Response) string {
	for _, h := range []string{"X-Request-Id", "X-Request-ID", "Request-Id"} {
		if v := resp.Header.Get(h); v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
