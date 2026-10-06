package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// JSON-RPC 2.0 over newline-delimited JSON, which is what ACP specifies for
// stdio.
//
// Hand-rolled rather than reused. The two JSON-RPC implementations already in
// the tree do not fit: internal/lsp is client-only, spawns the peer itself and
// frames with Content-Length headers, and the MCP SDK exports JSON-RPC *types*
// but keeps its connection and dispatch engine in an internal package that
// cannot be imported. What is left — a scanner, a pending-request map and a
// write mutex — is small, and the same shape is already working in
// internal/streamjson.
//
// Standard JSON-RPC error codes, plus the one ACP adds.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeAuthRequired is ACP's own: the agent needs authenticate before it
	// will serve session methods. Klaudia advertises no auth methods, so it
	// never sends this; the constant documents the code rather than reserving
	// it by accident.
	codeAuthRequired = -32000
)

// maxLine bounds one JSON-RPC line. An editor pasting a whole buffer as
// embedded context makes these genuinely large, so the limit is the same 16MB
// the stream-json frontend uses rather than bufio's 64KB default — which would
// silently truncate the read and then fail to parse.
const maxLine = 16 * 1024 * 1024

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

func errorf(code int, msg string) *rpcError { return &rpcError{Code: code, Message: msg} }

// rpcMessage is every frame on the wire: request, notification and response
// are distinguished by which fields are present, not by separate types.
//
// Method != "" is an inbound call; an id alongside it means a reply is owed.
// Method == "" is a response to something we sent.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// handler serves one inbound call. A nil result marshals as JSON null, which is
// what ACP's void responses are.
//
// A handler with work that must not happen until the client has its response
// returns an answer instead of a plain result.
type handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// answer is a result plus work deferred until the response has been written.
//
// session/new is why it exists. The client learns a new session's id *from the
// response*, so an available_commands_update sent before it names a session the
// client cannot yet route to, and the commands are dropped. Returning the
// notification as `after` makes the ordering exact, where a goroutine or a
// sleep would only make it likely.
type answer struct {
	result any
	after  func()
}

type conn struct {
	out    io.Writer
	handle handler

	mu      sync.Mutex // serializes writes to out
	pending sync.Map   // id string -> chan rpcMessage
	nextID  atomic.Int64
}

func newConn(out io.Writer, h handler) *conn {
	return &conn{out: out, handle: h}
}

// serve reads frames from r until EOF or a read error, dispatching each inbound
// call and routing each response to its waiter.
//
// Every inbound call runs on its own goroutine. That is not about throughput:
// session/prompt blocks for the length of a whole agent turn, and the two
// things that have to arrive *during* it — session/cancel, and the client's
// answer to session/request_permission — both come in on this same reader. A
// handler called inline would deadlock against the permission request it just
// sent.
func (c *conn) serve(ctx context.Context, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	// Wait for in-flight handlers before returning, so a client that closes
	// stdin does not strand a turn mid-write.
	var wg sync.WaitGroup
	defer wg.Wait()

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		// Unmarshalling copies every field out of the scanner's buffer
		// (json.RawMessage appends into a fresh slice), so the goroutine below
		// is safe to keep m past the next Scan.
		if err := json.Unmarshal(line, &m); err != nil {
			c.reply(nil, nil, errorf(codeParseError, "invalid JSON: "+err.Error()))
			continue
		}
		if m.Method == "" {
			c.deliver(m)
			continue
		}
		wg.Add(1)
		go func(m rpcMessage) {
			defer wg.Done()
			res, err := c.handle(ctx, m.Method, m.Params)
			var after func()
			if a, ok := res.(answer); ok {
				res, after = a.result, a.after
			}
			if after != nil {
				// Runs once the reply (or, for a notification, nothing) has
				// gone out. Deferred rather than called at each return below,
				// so an error path cannot skip it.
				defer after()
			}
			if len(m.ID) == 0 {
				// A notification. JSON-RPC forbids a reply even when the
				// handler failed, so the error has nowhere to go but the
				// agent's own diagnostics.
				return
			}
			if err != nil {
				c.reply(m.ID, nil, asRPCError(err))
				return
			}
			c.reply(m.ID, res, nil)
		}(m)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return nil
}

// asRPCError keeps a handler's own code when it set one and falls back to
// "internal error" otherwise, so a stray Go error cannot be reported to the
// client as a protocol violation it could fix.
func asRPCError(err error) *rpcError {
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	return errorf(codeInternalError, err.Error())
}

func (c *conn) reply(id json.RawMessage, result any, rerr *rpcError) {
	msg := rpcMessage{JSONRPC: "2.0", ID: id, Error: rerr}
	if rerr == nil {
		b, err := json.Marshal(result)
		if err != nil {
			msg.Error = errorf(codeInternalError, "could not encode result: "+err.Error())
		} else {
			msg.Result = b
		}
	}
	if len(msg.ID) == 0 {
		msg.ID = json.RawMessage("null")
	}
	c.write(msg)
}

// notify sends a one-way message. ACP's session/update is a notification, so
// this is the hot path: it must not wait for anything.
func (c *conn) notify(method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		return
	}
	c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: b})
}

// call sends a request and blocks until the client answers it or ctx is done.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := strconv.FormatInt(c.nextID.Add(1), 10)
	ch := make(chan rpcMessage, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	c.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: b})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result == nil || len(m.Result) == 0 {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
}

// deliver hands a response to whoever is waiting on its id. An unknown id is
// dropped: it belongs to a request whose caller has already given up.
func (c *conn) deliver(m rpcMessage) {
	ch, ok := c.pending.LoadAndDelete(string(m.ID))
	if !ok {
		return
	}
	ch.(chan rpcMessage) <- m
}

func (c *conn) write(m rpcMessage) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.out.Write(b)
	_, _ = c.out.Write([]byte("\n"))
}
