package api

import (
	"encoding/json"
	"log"
	"time"
)

// modelCall is one attempt at a model request, logged as a single JSON line.
// It records status and shape only: never the API key, an auth header, the
// prompt, or tool arguments. The line goes to the standard logger, which the
// TUI redirects — set KLAUDIA_LOG to a file path to keep it (see
// tui.quietStandardLogger); without that it is discarded so it cannot corrupt
// the frame. One JSON object per line is what promtail and Grafana Alloy scrape
// directly.
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
	log.Printf("klaudia %s", b)
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
