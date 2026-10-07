package api

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// modelLog is where model-call lines go. stderr, not the standard logger: the
// TUI discards log.Default so a library's log.Printf cannot corrupt the frame
// (tui.quietStandardLogger), which would also swallow these. Docker captures a
// container's stderr, and Node B's promtail scrapes every container's log
// stream, so a line written here is in Loki with no extra shipper. Tests
// replace it.
var (
	modelLogMu sync.Mutex
	modelLog   = os.Stderr
)

// modelCall is one attempt at a model request, logged as a single JSON line.
// It records status and shape only: never the API key, an auth header, the
// prompt, or tool arguments.
type modelCall struct {
	session   string
	host      string
	model     string
	attempt   int
	max       int
	status    int
	requestID string
	latency   time.Duration
	// tokens are filled in by the stream consumer once usage arrives; the
	// per-attempt line is logged before the body is read, so they are usually
	// absent there and present on the completion line.
	inTokens, outTokens int
	retry               bool
	err                 string
}

func logModelCall(c modelCall) {
	line := map[string]any{
		"event":    "model_call",
		"provider": "openai",
		"host":     c.host,
		"model":    c.model,
		"attempt":  c.attempt,
		"max":      c.max,
		"latency":  c.latency.Milliseconds(),
	}
	if c.session != "" {
		line["session"] = c.session
	}
	if c.status != 0 {
		line["status"] = c.status
		line["status_class"] = statusClass(c.status)
	}
	if c.requestID != "" {
		line["request_id"] = c.requestID
	}
	if c.inTokens != 0 || c.outTokens != 0 {
		line["input_tokens"] = c.inTokens
		line["output_tokens"] = c.outTokens
	}
	if c.retry {
		line["retry"] = true
	}
	if c.err != "" {
		line["error"] = c.err
	}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	modelLogMu.Lock()
	defer modelLogMu.Unlock()
	_, _ = modelLog.Write(append(b, '\n'))
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 200:
		return "2xx"
	default:
		return "other"
	}
}

// dumpFailedRequest writes a redacted copy of a request the endpoint rejected,
// so the body that failed can be diffed against one that succeeded. It is off
// unless KLAUDIA_DUMP_FAILED_REQUEST names a file. The dump drops every header
// (so no auth), replaces message text and tool-call arguments with their
// lengths, and truncates anything else oversized. What remains is the shape:
// roles, tool names, schema structure, and sizes.
func dumpFailedRequest(status int, body []byte) {
	path := os.Getenv("KLAUDIA_DUMP_FAILED_REQUEST")
	if path == "" {
		return
	}
	redacted := redactRequest(body)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec, _ := json.Marshal(map[string]any{
		"event":   "failed_request",
		"status":  status,
		"request": json.RawMessage(redacted),
	})
	_, _ = f.Write(append(rec, '\n'))
}

// redactLimit caps any string that isn't already replaced by its length, so a
// schema with a huge description can't fill the dump.
const redactLimit = 300

func redactRequest(body []byte) []byte {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return []byte(`{"unparseable":true}`)
	}
	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			redactMessage(m)
		}
	}
	if tools, ok := req["tools"].([]any); ok {
		for _, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tool["function"].(map[string]any); ok {
				if d, ok := fn["description"].(string); ok {
					fn["description"] = truncate(d, redactLimit)
				}
			}
		}
	}
	out, err := json.Marshal(req)
	if err != nil {
		return []byte(`{"unparseable":true}`)
	}
	return out
}

func redactMessage(m any) {
	msg, ok := m.(map[string]any)
	if !ok {
		return
	}
	switch c := msg["content"].(type) {
	case string:
		msg["content"] = lengthMark(c)
	case []any:
		for _, part := range c {
			redactContentPart(part)
		}
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := call["function"].(map[string]any); ok {
				if args, ok := fn["arguments"].(string); ok {
					fn["arguments"] = lengthMark(args)
				}
			}
		}
	}
}

func redactContentPart(part any) {
	p, ok := part.(map[string]any)
	if !ok {
		return
	}
	if t, ok := p["text"].(string); ok {
		p["text"] = lengthMark(t)
	}
	// An image part carries the picture inline as a data URL.
	if img, ok := p["image_url"].(map[string]any); ok {
		if u, ok := img["url"].(string); ok {
			img["url"] = lengthMark(u)
		}
	}
}

func lengthMark(s string) string { return "[redacted " + itoa(len(s)) + " chars]" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
