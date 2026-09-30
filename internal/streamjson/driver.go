// Package streamjson implements the bidirectional stream-json frontend: a
// persistent agent driven over stdin/stdout with newline-delimited JSON. This
// is the embedding channel used by SDKs and editor integrations (e.g. Zed/ACP
// can build on it) — no terminal required.
//
// Protocol (matching the JS reference):
//
//	in:  {"type":"user","message":{"role":"user","content":"..."}}
//	in:  {"type":"control_response","response":{"subtype":"success",
//	      "request_id":"<id>","response":{"behavior":"allow"|"deny",...}}}
//	out: {"type":"assistant"|"user","message":{"role":...,"content":[...]},
//	      "session_id":"<id>","parent_tool_use_id":null,"uuid":"<id>"}
//	out: usage / tool_progress / compaction events (agent.Event)
//	out: {"type":"control_request","request_id":"<id>",
//	      "request":{"subtype":"can_use_tool","tool_name":"...","input":{...}}}
//	out: {"type":"result", ...}
//	in:  {"type":"control_request","request_id":"<id>",
//	      "request":{"subtype":"interrupt"|"set_permission_mode"|"set_model"|"initialize",...}}
//	out: {"type":"control_response","response":{"subtype":"success"|"error",
//	      "request_id":"<id>","response":{...}|"error":"..."}}
//
// Conversation content — assistant text, tool_use blocks, tool_result blocks —
// travels in the message envelope, the same line the single-shot `-p
// --output-format stream-json` path and the JS reference emit. It used to be
// written here as Klaudia's flat agent.Event lines ({"type":"assistant","text":
// ...}, {"type":"tool_use",...}, {"type":"tool_result",...}) instead, so the
// two stream-json outputs of one binary disagreed on the shape of an assistant
// message, and a client written against the documented one saw nothing of a
// turn until its result line. The Driver is the run's Recorder for that reason:
// every message the loop records is also the message the peer is shown.
//
// A can_use_tool request is emitted only for calls the permission flow could
// not settle on its own — a tool on the config allow list, or one denied by a
// deny rule, never reaches the peer. What does reach it must be answered: the
// agent turn blocks on the reply. The wait is bounded by Driver.AskTimeout
// (DefaultAskTimeout unless the CLI overrides it), after which the ask is
// denied with a message saying so; a peer that does not implement the control
// protocol therefore sees a denied tool and a finished turn, not a hung
// process.
package streamjson

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/permission"
)

// RunFunc runs one user turn to completion, seeded with prior conversation
// history, using the supplied approver, recorder and emitter. It returns the
// agent Result whose Messages field carries the updated history forward. The
// CLI provides this, wiring in the API client, tools, model, and permission
// context. rec must be given to the loop as (part of) its Recorder: it is how
// the conversation reaches the peer, and a run that drops it streams no
// assistant text at all.
type RunFunc func(ctx context.Context, prompt string, history []anthropic.BetaMessageParam, approver agent.Approver, rec agent.Recorder, emit agent.Emitter) (agent.Result, error)

// inMessage is a decoded stdin line.
type inMessage struct {
	Type     string          `json:"type"`
	Message  json.RawMessage `json:"message,omitempty"`
	Response *controlResp    `json:"response,omitempty"`
	// RequestID and Request are set on a control_request from the peer.
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`

	// seq numbers user lines in the order they were read (from 1), so an
	// interrupt can reach a turn that was read but has not started yet.
	seq uint64
}

type controlResp struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// permissionAnswer is the payload inside a can_use_tool control response.
type permissionAnswer struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

// DefaultAskTimeout bounds how long a can_use_tool request waits for its
// control_response when the CLI does not say otherwise.
//
// The JS reference waits forever, and so did this driver. Forever is the wrong
// default for an embedding channel: the peer is a program, and a program that
// never answers — because it was written against the config allow list and
// never implemented control responses, or because it stalled — left the agent
// wedged mid-turn with no output and no exit. Ten minutes is long enough for a
// human behind an editor integration to read a prompt and decide, and short
// enough that an unattended pipeline fails visibly within the run rather than
// at whatever outer timeout kills it.
const DefaultAskTimeout = 10 * time.Minute

// Driver runs the stream-json protocol over a reader/writer pair.
type Driver struct {
	out     io.Writer
	mu      sync.Mutex // serializes writes to out
	pending sync.Map   // request_id -> chan permission.Decision

	// SessionID is stamped on every message envelope, as the JS reference does,
	// so a peer can tell which conversation a line belongs to. The CLI sets it
	// to the transcript's session id.
	SessionID string

	// AskTimeout bounds the wait for a control_response to each can_use_tool
	// request; an unanswered request is denied when it elapses. Zero or
	// negative means wait until the context ends, which is the pre-timeout
	// behaviour and the JS reference's.
	AskTimeout time.Duration

	// History seeds the conversation the first turn runs with: the messages
	// of a resumed session. Nil starts empty.
	History []anthropic.BetaMessageParam

	// Init, when set, is announced as the first output line,
	// {"type":"system","subtype":"init","session_id":...}, so a peer learns
	// the session id (and whether it resumed) before any turn is sent.
	Init *Init

	// SetPermissionMode applies a set_permission_mode control request. It
	// returns an error, which the peer receives as an error control_response,
	// for a mode that is unknown or not permitted in this session. Nil answers
	// every such request with an error.
	SetPermissionMode func(mode string) error

	// SetModel applies a set_model control request for the turns that follow,
	// returning the model now in effect. An empty model restores the session's
	// launch model. Nil answers every such request with an error.
	SetModel func(model string) (string, error)

	// Turn state, shared between the stdin reader (which handles interrupt)
	// and the turn loop.
	turnMu      sync.Mutex
	readSeq     uint64             // user lines read so far
	interruptTo uint64             // user lines numbered <= this are interrupted
	cancelTurn  context.CancelFunc // cancels the running turn; nil when idle
	initialized bool               // an initialize request has been answered
}

// Init describes the session in the system/init line.
type Init struct {
	CWD            string
	Model          string
	PermissionMode string
	// ResumedFrom is the session id whose history was loaded; empty for a
	// fresh session. It differs from SessionID for a fork.
	ResumedFrom string
}

// NewDriver builds a Driver writing to w, waiting DefaultAskTimeout for each
// permission answer.
func NewDriver(w io.Writer) *Driver {
	return &Driver{out: w, AskTimeout: DefaultAskTimeout}
}

// Run reads messages from r until EOF, running the agent for each user message.
// Permission asks are emitted as control_request lines and block until the
// peer sends the matching control_response, or AskTimeout passes.
//
// Stdin is read on its own goroutine for the whole run, and control requests
// from the peer are answered there, not in the turn loop: an interrupt has to
// reach a turn that is running, and a set_permission_mode sent mid-turn applies
// from the turn's next tool call. User lines queue for the turn loop without
// bound, so a peer that sends ahead can never stop the reader from seeing an
// interrupt or a control_response behind them.
func (d *Driver) Run(ctx context.Context, r io.Reader, run RunFunc) error {
	q := newTurnQueue()

	go func() {
		defer q.close()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var m inMessage
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				continue
			}
			switch m.Type {
			case "control_response":
				d.deliverControlResponse(m.Response)
			case "control_request":
				d.handleControlRequest(m.RequestID, m.Request)
			case "user":
				d.turnMu.Lock()
				d.readSeq++
				m.seq = d.readSeq
				d.turnMu.Unlock()
				q.push(m)
			}
		}
	}()

	if d.Init != nil {
		line := map[string]any{
			"type":             "system",
			"subtype":          "init",
			"session_id":       d.SessionID,
			"cwd":              d.Init.CWD,
			"model":            d.Init.Model,
			"permissionMode":   d.Init.PermissionMode,
			"resumed":          d.Init.ResumedFrom != "",
			"history_messages": len(d.History),
		}
		if d.Init.ResumedFrom != "" {
			line["resumed_from"] = d.Init.ResumedFrom
		}
		d.write(line)
	}

	approver := &controlApprover{driver: d}
	history := d.History
	for {
		m, ok, err := q.next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil // stdin closed and every queued turn run
		}
		prompt := decodeUserContent(m.Message)
		if prompt == "" {
			continue
		}
		start := time.Now()
		turnCtx, cancel := context.WithCancel(ctx)
		if !d.beginTurn(m.seq, cancel) {
			// Interrupted before it started: the peer still gets the turn's
			// result line, so every user line it sent is accounted for.
			cancel()
			d.write(d.resultEvent(agent.Result{}, context.Canceled, time.Since(start), true))
			continue
		}
		// Conversation content reaches the peer through Record, in the
		// envelope. The flat events carrying the same text, tool calls
		// and results are dropped here so nothing arrives twice in two
		// shapes; what remains are the events that have no message form.
		emit := func(ev agent.Event) {
			switch ev.Type {
			case "assistant", "tool_use", "tool_result":
				return
			}
			d.write(ev)
		}
		res, err := run(turnCtx, prompt, history, approver, d, emit)
		interrupted := d.endTurn(m.seq) && err != nil
		cancel()
		if res.Messages != nil {
			history = res.Messages // carry conversation forward
		}
		d.write(d.resultEvent(res, err, time.Since(start), interrupted))
	}
}

// beginTurn registers cancel as the running turn's, unless an interrupt has
// already covered user line seq, in which case it reports false and the turn
// must not run.
func (d *Driver) beginTurn(seq uint64, cancel context.CancelFunc) bool {
	d.turnMu.Lock()
	defer d.turnMu.Unlock()
	if seq <= d.interruptTo {
		return false
	}
	d.cancelTurn = cancel
	return true
}

// endTurn clears the running turn and reports whether an interrupt covered it.
func (d *Driver) endTurn(seq uint64) bool {
	d.turnMu.Lock()
	defer d.turnMu.Unlock()
	d.cancelTurn = nil
	return seq <= d.interruptTo
}

// interrupt cancels the running turn — the per-turn cancel the TUI's Esc uses —
// and every turn read before the interrupt that has not started yet.
// Cancelling the turn's context also releases a can_use_tool ask it is parked
// on. With nothing running or queued it is a no-op.
func (d *Driver) interrupt() {
	d.turnMu.Lock()
	d.interruptTo = d.readSeq
	cancel := d.cancelTurn
	d.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// handleControlRequest answers a control_request from the peer with a
// control_response carrying the same request_id: success with the subtype's
// payload, or error with a message. The subtypes and shapes are Claude Code's.
func (d *Driver) handleControlRequest(id string, raw json.RawMessage) {
	if id == "" {
		return // nothing to answer to
	}
	var req struct {
		Subtype string  `json:"subtype"`
		Mode    string  `json:"mode"`
		Model   *string `json:"model"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		d.controlError(id, "malformed control request: "+err.Error())
		return
	}
	switch req.Subtype {
	case "interrupt":
		d.interrupt()
		d.controlSuccess(id, nil)
	case "set_permission_mode":
		if d.SetPermissionMode == nil {
			d.controlError(id, "set_permission_mode is not supported by this session")
			return
		}
		if err := d.SetPermissionMode(req.Mode); err != nil {
			d.controlError(id, err.Error())
			return
		}
		d.controlSuccess(id, map[string]any{"mode": req.Mode})
	case "set_model":
		if d.SetModel == nil {
			d.controlError(id, "set_model is not supported by this session")
			return
		}
		// Claude Code sends no model, or "default", to go back to the default.
		want := ""
		if req.Model != nil && *req.Model != "default" {
			want = *req.Model
		}
		model, err := d.SetModel(want)
		if err != nil {
			d.controlError(id, err.Error())
			return
		}
		d.controlSuccess(id, map[string]any{"model": model})
	case "initialize":
		d.initialize(id, raw)
	default:
		d.controlError(id, fmt.Sprintf("unsupported control request subtype %q", req.Subtype))
	}
}

// initializeUnsupported are the initialize fields Claude Code's SDKs use to
// hand the CLI callbacks or configuration Klaudia has no way to honour. A
// request carrying one is refused rather than accepted and silently ignored:
// a PreToolUse hook that never runs is a guardrail the embedder believes is in
// place and is not.
var initializeUnsupported = []string{"hooks", "sdkMcpServers", "jsonSchema", "systemPrompt", "appendSystemPrompt", "agents"}

// initialize answers the SDK handshake. Klaudia has nothing to negotiate, so
// the reply carries the fields Claude Code's reply carries, empty where Klaudia
// has no equivalent; its value to a peer is that an SDK waits for this answer
// before it sends a turn.
func (d *Driver) initialize(id string, raw json.RawMessage) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var refused []string
	for _, k := range initializeUnsupported {
		if !emptyJSON(fields[k]) {
			refused = append(refused, k)
		}
	}
	if len(refused) > 0 {
		d.controlError(id, fmt.Sprintf("initialize: Klaudia does not support %s over stream-json; "+
			"configure hooks, MCP servers, agents and prompts in Klaudia's own config instead",
			strings.Join(refused, ", ")))
		return
	}
	d.turnMu.Lock()
	again := d.initialized
	d.initialized = true
	d.turnMu.Unlock()
	if again {
		d.controlError(id, "already initialized")
		return
	}
	d.controlSuccess(id, map[string]any{
		"commands":                []any{},
		"models":                  []any{},
		"output_style":            "default",
		"available_output_styles": []string{"default"},
	})
}

// emptyJSON reports whether v is absent, null, or an empty string, object or
// array: the values an SDK sends for an option the caller did not set.
func emptyJSON(v json.RawMessage) bool {
	switch string(bytes.TrimSpace(v)) {
	case "", "null", `""`, "{}", "[]":
		return true
	}
	return false
}

// controlSuccess writes a success control_response for request id.
func (d *Driver) controlSuccess(id string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	d.write(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": id,
			"response":   payload,
		},
	})
}

// controlError writes an error control_response for request id.
func (d *Driver) controlError(id, msg string) {
	d.write(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "error",
			"request_id": id,
			"error":      msg,
		},
	})
}

// turnQueue holds user lines between the stdin reader and the turn loop. It
// is unbounded so the reader never blocks on it: a blocked reader is one that
// cannot see an interrupt, or the control_response a running turn waits on.
type turnQueue struct {
	mu     sync.Mutex
	items  []inMessage
	closed bool
	ready  chan struct{} // signalled, never blocking, on push and close
}

func newTurnQueue() *turnQueue { return &turnQueue{ready: make(chan struct{}, 1)} }

func (q *turnQueue) push(m inMessage) {
	q.mu.Lock()
	q.items = append(q.items, m)
	q.mu.Unlock()
	q.signal()
}

func (q *turnQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *turnQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// next returns the oldest queued line, waiting for one. ok is false once the
// queue is closed and drained; err is set if ctx ends first.
func (q *turnQueue) next(ctx context.Context) (m inMessage, ok bool, err error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			m = q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return m, true, nil
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return inMessage{}, false, nil
		}
		select {
		case <-ctx.Done():
			return inMessage{}, false, ctx.Err()
		case <-q.ready:
		}
	}
}

// Record implements agent.Recorder: each conversation message the loop
// records is written to the peer as a JS-compatible envelope
// {type, message, session_id, parent_tool_use_id, uuid} — the same line
// cli's envelopeRecorder produces for `-p --output-format stream-json`.
func (d *Driver) Record(role string, message json.RawMessage) error {
	d.write(map[string]any{
		"type":               role, // "user" | "assistant"
		"message":            message,
		"session_id":         d.SessionID,
		"parent_tool_use_id": nil,
		"uuid":               uuid.NewString(),
	})
	return nil
}

// write emits one JSON line, serialized against concurrent writers.
func (d *Driver) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.out.Write(b)
	_, _ = d.out.Write([]byte("\n"))
}

// deliverControlResponse resolves a pending permission request.
func (d *Driver) deliverControlResponse(resp *controlResp) {
	if resp == nil || resp.RequestID == "" {
		return
	}
	ch, ok := d.pending.LoadAndDelete(resp.RequestID)
	if !ok {
		return
	}
	decision := permission.Decision{Behavior: permission.Deny, Message: "denied"}
	if resp.Subtype == "success" && len(resp.Response) > 0 {
		var ans permissionAnswer
		if json.Unmarshal(resp.Response, &ans) == nil {
			if ans.Behavior == "allow" {
				decision = permission.Decision{Behavior: permission.Allow}
			} else {
				decision = permission.Decision{Behavior: permission.Deny, Message: ans.Message}
			}
		}
	}
	ch.(chan permission.Decision) <- decision
}

// controlApprover emits a control_request and waits for the peer's answer.
type controlApprover struct {
	driver *Driver
}

func (a *controlApprover) Approve(ctx context.Context, req agent.ApprovalRequest) permission.Decision {
	id := uuid.NewString()
	ch := make(chan permission.Decision, 1)
	a.driver.pending.Store(id, ch)
	defer a.driver.pending.Delete(id)

	a.driver.write(map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request": map[string]any{
			"subtype":   "can_use_tool",
			"tool_name": req.ToolName,
			"input":     json.RawMessage(req.Input),
		},
	})

	// A nil channel never fires, so no timeout means the old unbounded wait.
	var timeout <-chan time.Time
	if a.driver.AskTimeout > 0 {
		timer := time.NewTimer(a.driver.AskTimeout)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case <-ctx.Done():
		return permission.Decision{Behavior: permission.Deny, Message: "cancelled"}
	case dec := <-ch:
		return dec
	case <-timeout:
		// The deferred Delete drops the waiter, so an answer that arrives after
		// this is discarded by deliverControlResponse rather than misapplied.
		return permission.Decision{
			Behavior: permission.Deny,
			Message: fmt.Sprintf("Permission for tool %s was requested from the embedding client "+
				"(control_request can_use_tool) but no control_response arrived within %s; denied. "+
				"The client must answer can_use_tool requests, or pre-approve the tool with a "+
				"[permissions] allow rule so it is never asked.", req.ToolName, a.driver.AskTimeout),
		}
	}
}

// decodeUserContent extracts the text of a user message whose content is either
// a string or an array of content blocks.
func decodeUserContent(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) == nil {
		var out string
		for _, b := range blocks {
			if b.Type == "text" {
				out += b.Text
			}
		}
		return out
	}
	return ""
}

// resultEvent builds the terminal result line for a turn.
//
// It carries the turn's token usage in the same shape as the -p path's
// ResultMessage (cli/headless.go), so an embedder can account for a turn from
// the result line alone. Before this the embedding channel's result had no
// usage at all; the per-call "usage" delta events were the only counts an
// embedder ever saw, and one that only read the result (msp-agent) reported
// 0→0 tokens for every turn.
//
// session_id and duration_ms are the -p result's fields of the same names.
// total_cost_usd is left out rather than sent as the -p path's placeholder 0:
// Klaudia does not price a turn yet, and a zero reads as "this was free".
//
// An interrupted turn reports error_during_execution with a result that says
// it was interrupted, rather than the bare context error.
func (d *Driver) resultEvent(res agent.Result, err error, dur time.Duration, interrupted bool) map[string]any {
	m := map[string]any{
		"type":           "result",
		"subtype":        "success",
		"is_error":       err != nil,
		"duration_ms":    dur.Milliseconds(),
		"num_turns":      res.NumTurns,
		"result":         res.Text,
		"stop_reason":    res.StopReason,
		"session_id":     d.SessionID,
		"total_cost_usd": res.CostUSD,
		"usage": map[string]any{
			"input_tokens":                res.InputTokens,
			"output_tokens":               res.OutputTokens,
			"cache_read_input_tokens":     res.CacheReadInputTokens,
			"cache_creation_input_tokens": res.CacheCreationInputTokens,
		},
	}
	switch {
	case interrupted:
		m["subtype"] = "error_during_execution"
		m["result"] = "Interrupted by the client (control_request interrupt)."
	case err != nil:
		m["subtype"] = "error_during_execution"
		m["result"] = "Error: " + api.FriendlyError(err)
	}
	return m
}
