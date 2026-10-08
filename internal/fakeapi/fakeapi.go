// Package fakeapi is a scripted stand-in for the Anthropic Messages API, for
// tests. It serves /v1/messages on a loopback port, answering each request with
// the next scripted Turn as a streamed (SSE) response, and records what it was
// sent so a test can assert on the requests klaudia made.
//
// Two layers use it: internal/cli tests, which run the command in-process, and
// the e2e suite, which runs the built binary. Point klaudia at it with
// KLAUDIA_CUSTOM_ENDPOINT=srv.URL() and any non-empty ANTHROPIC_API_KEY.
//
// It is test support, not product code: nothing outside tests imports it.
package fakeapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Turn is one scripted assistant response: optional text followed by zero or
// more tool calls. A Turn with tool calls ends with stop_reason tool_use,
// otherwise end_turn, unless StopReason overrides it.
type Turn struct {
	Text       string
	Tools      []ToolCall
	StopReason string

	// Status, when non-zero, makes the server answer with this HTTP status and
	// an Anthropic error body of type ErrType instead of a stream.
	Status  int
	ErrType string

	// Delay, when non-zero, holds the response back this long, for a test
	// that needs one caller (a background sub-agent, say) to be slower than
	// another.
	Delay time.Duration
}

// ToolCall is a tool_use block the scripted model emits.
type ToolCall struct {
	Name  string
	Input map[string]any
}

// Say is a turn that only replies with text.
func Say(text string) Turn { return Turn{Text: text} }

// Use is a turn that calls one tool.
func Use(name string, input map[string]any) Turn {
	return Turn{Tools: []ToolCall{{Name: name, Input: input}}}
}

// Fail is a turn the API rejects with an HTTP error.
func Fail(status int, errType string) Turn { return Turn{Status: status, ErrType: errType} }

// Request is one /v1/messages call as the server received it.
type Request struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Tools    []struct {
		Name string `json:"name"`
	} `json:"tools"`
	raw []byte
}

// FirstMessage is the first message of the conversation, verbatim. For a
// sub-agent's request that is the prompt its parent gave it, which is what
// Route matches on.
func (r Request) FirstMessage() string {
	if len(r.Messages) == 0 {
		return ""
	}
	return string(r.Messages[0])
}

// Raw is the request body verbatim, for substring assertions.
func (r Request) Raw() string { return string(r.raw) }

// ToolNames lists the tools offered to the model in this request.
func (r Request) ToolNames() []string {
	var names []string
	for _, t := range r.Tools {
		names = append(names, t.Name)
	}
	return names
}

// Server serves the script. When the script runs out it answers "done." so a
// run that takes more turns than expected ends cleanly and the test fails on
// its assertions rather than on a hang.
type Server struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	script   []Turn
	routes   []*route
	requests []Request
}

// route is a separate script for the requests whose first message contains
// match: one conversation's turns, kept apart from the others'.
type route struct {
	match  string
	script []Turn
}

// New starts a server with the given script. It is closed when the test ends.
func New(t testing.TB, script ...Turn) *Server {
	t.Helper()
	s := &Server{t: t, script: script}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base URL to give klaudia as KLAUDIA_CUSTOM_ENDPOINT.
func (s *Server) URL() string { return s.srv.URL }

// Script appends turns, for tests that learn a path only after setup or that
// share one server across several runs.
func (s *Server) Script(turns ...Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, turns...)
}

// Route answers the requests whose first message contains match from their
// own script, ahead of the main one. A sub-agent's first message is the prompt
// its parent wrote, so a marker in that prompt routes the child's requests here
// — which is what makes a run with concurrent sub-agents deterministic: they
// and the parent call the API in no fixed order, and one shared script would
// hand each whichever turn came next. A route whose script runs out answers
// "done.". The first matching route wins.
func (s *Server) Route(match string, turns ...Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append(s.routes, &route{match: match, script: turns})
}

// Requests returns every /v1/messages call received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/messages") || r.Method != http.MethodPost {
		// Anything else is a request the fake doesn't model. Fail loudly so a
		// new startup call shows up as a clear test failure, not a mystery hang.
		s.t.Errorf("fakeapi: unexpected %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"not modelled by fakeapi"}}`, http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.t.Errorf("fakeapi: decoding request: %v", err)
	}
	req.raw = body

	s.mu.Lock()
	s.requests = append(s.requests, req)
	seq := len(s.requests)
	turn := Say("done.")
	script := &s.script
	for _, rt := range s.routes {
		if strings.Contains(req.FirstMessage(), rt.match) {
			script = &rt.script
			break
		}
	}
	if len(*script) > 0 {
		turn, *script = (*script)[0], (*script)[1:]
	}
	s.mu.Unlock()

	if turn.Delay > 0 {
		select {
		case <-time.After(turn.Delay):
		case <-r.Context().Done():
			return
		}
	}

	if turn.Status != 0 {
		errType := turn.ErrType
		if errType == "" {
			errType = "invalid_request_error"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(turn.Status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":"scripted failure"}}`, errType)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	writeTurn(w, seq, req.Model, turn)
}

// writeTurn streams one turn as Anthropic SSE events.
func writeTurn(w io.Writer, seq int, model string, turn Turn) {
	event := func(name string, data map[string]any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": fmt.Sprintf("msg_fake_%d", seq), "type": "message", "role": "assistant",
		"model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 0},
	}})

	idx := 0
	if turn.Text != "" {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "text_delta", "text": turn.Text}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	for i, tc := range turn.Tools {
		input, _ := json.Marshal(tc.Input)
		event("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_fake_%d_%d", seq, i),
				"name": tc.Name, "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}

	stop := "end_turn"
	if len(turn.Tools) > 0 {
		stop = "tool_use"
	}
	if turn.StopReason != "" {
		stop = turn.StopReason
	}
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 5}})
	event("message_stop", map[string]any{"type": "message_stop"})
}
