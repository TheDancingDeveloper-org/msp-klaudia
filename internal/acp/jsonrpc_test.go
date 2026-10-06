package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// frameSink is an Agent's stdout: it splits the newline-delimited frames the
// conn writes and buffers them on a channel.
//
// A channel rather than a pipe deliberately. An io.Pipe blocks the writer until
// someone reads, so a test that has not got round to reading a session/update
// yet would deadlock the turn that emitted it — turning an ordering bug in the
// test into a hang in the agent.
type frameSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	frames chan rpcMessage
}

func newFrameSink() *frameSink {
	return &frameSink{frames: make(chan rpcMessage, 256)}
}

func (s *frameSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadBytes('\n')
		if err != nil {
			// Not a whole frame yet: conn.write emits the body and the newline
			// as two Writes, so a partial read here is normal.
			s.buf.Write(line)
			break
		}
		var m rpcMessage
		if json.Unmarshal(bytes.TrimSpace(line), &m) != nil {
			continue
		}
		s.frames <- m
	}
	return len(p), nil
}

func (s *frameSink) next(t *testing.T) rpcMessage {
	t.Helper()
	select {
	case m := <-s.frames:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return rpcMessage{}
	}
}

func TestConnServesRequestsAndNotifications(t *testing.T) {
	sink := newFrameSink()
	var mu sync.Mutex
	var seen []string
	c := newConn(sink, func(_ context.Context, method string, params json.RawMessage) (any, error) {
		mu.Lock()
		seen = append(seen, method+" "+string(params))
		mu.Unlock()
		switch method {
		case "ping":
			return map[string]string{"pong": "yes"}, nil
		case "boom":
			return nil, errorf(codeInvalidParams, "no good")
		case "oops":
			return nil, errors.New("a plain Go error")
		}
		return nil, nil
	})

	in := bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":1}}` + "\n" +
			`{"jsonrpc":"2.0","method":"note","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"boom"}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"oops"}` + "\n" +
			`not json at all` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"void"}` + "\n",
	)
	if err := c.serve(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	// Replies may arrive in any order: each inbound call runs on its own
	// goroutine, which is what keeps session/cancel readable during a prompt.
	// The parse error is in here too, under the null id it has to use because
	// there was no id to echo.
	got := map[string]rpcMessage{}
	for i := 0; i < 5; i++ {
		m := sink.next(t)
		got[string(m.ID)] = m
	}

	if res := got["1"]; string(res.Result) != `{"pong":"yes"}` {
		t.Errorf("ping result = %s", res.Result)
	}
	if res := got["2"]; res.Error == nil || res.Error.Code != codeInvalidParams {
		t.Errorf("boom error = %+v, want the handler's own code", res.Error)
	}
	if res := got["3"]; res.Error == nil || res.Error.Code != codeInternalError {
		// A stray Go error must not be reported as a protocol violation the
		// client could fix.
		t.Errorf("oops error = %+v, want internal error", res.Error)
	}
	if res := got["4"]; res.Error != nil || string(res.Result) != "null" {
		t.Errorf("void result = %s / %+v, want null", res.Result, res.Error)
	}
	if m, ok := got["null"]; !ok || m.Error == nil || m.Error.Code != codeParseError {
		t.Errorf("parse error frame = %+v", m)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 5 {
		t.Errorf("handler saw %d calls (%v), want 5 — the notification included", len(seen), seen)
	}
}

func TestConnDoesNotReplyToANotification(t *testing.T) {
	// JSON-RPC forbids a reply to a notification even when the handler failed.
	// session/cancel is a notification, so getting this wrong would put an
	// unanswerable error frame in front of the client every time a user hit
	// stop.
	sink := newFrameSink()
	c := newConn(sink, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("handler failed")
	})
	in := bytes.NewBufferString(`{"jsonrpc":"2.0","method":"session/cancel","params":{}}` + "\n")
	if err := c.serve(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-sink.frames:
		t.Errorf("got a frame for a notification: %+v", m)
	default:
	}
}

func TestConnCallAwaitsItsOwnResponse(t *testing.T) {
	// The reply to an outbound request arrives on the same reader that is
	// serving inbound calls. If a handler ran inline, a handler that called out
	// would deadlock against its own answer — which is exactly what
	// session/prompt does when it asks for permission.
	sink := newFrameSink()
	inR, inW := io.Pipe()

	var c *conn
	answered := make(chan string, 1)
	c = newConn(sink, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method != "slow" {
			return nil, nil
		}
		var res struct {
			Value string `json:"value"`
		}
		if err := c.call(ctx, "ask", map[string]string{"q": "?"}, &res); err != nil {
			return nil, err
		}
		answered <- res.Value
		return map[string]string{"got": res.Value}, nil
	})

	done := make(chan error, 1)
	go func() { done <- c.serve(context.Background(), inR) }()

	_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))

	// The agent's outbound request comes out first.
	out := sink.next(t)
	if out.Method != "ask" {
		t.Fatalf("first frame = %+v, want the outbound ask", out)
	}
	_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":` + string(out.ID) + `,"result":{"value":"42"}}` + "\n"))

	select {
	case v := <-answered:
		if v != "42" {
			t.Errorf("answer = %q, want 42", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never got its answer — the reader is blocked behind it")
	}
	if reply := sink.next(t); string(reply.Result) != `{"got":"42"}` {
		t.Errorf("reply = %s", reply.Result)
	}

	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConnCallHonoursContext(t *testing.T) {
	// A client that never answers must not strand the turn: the turn's context
	// is cancelled (session/cancel, or stdin closing) and the call returns.
	sink := newFrameSink()
	c := newConn(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.call(ctx, "ask", map[string]string{}, nil) }()
	sink.next(t) // the request went out
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call did not return after its context was cancelled")
	}
}

func TestConnCallSurfacesAClientError(t *testing.T) {
	sink := newFrameSink()
	c := newConn(sink, nil)
	errc := make(chan error, 1)
	go func() { errc <- c.call(context.Background(), "ask", map[string]string{}, nil) }()
	out := sink.next(t)
	c.deliver(rpcMessage{ID: out.ID, Error: errorf(codeMethodNotFound, "unsupported")})
	err := <-errc
	var re *rpcError
	if !errors.As(err, &re) || re.Code != codeMethodNotFound {
		t.Errorf("err = %v, want a method-not-found rpcError", err)
	}
}

func TestConnIgnoresAnUnknownResponseID(t *testing.T) {
	// A response for a request whose caller has given up (the turn was
	// cancelled) must be dropped, not block the reader forever.
	sink := newFrameSink()
	c := newConn(sink, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	in := bytes.NewBufferString(`{"jsonrpc":"2.0","id":999,"result":{}}` + "\n")
	done := make(chan error, 1)
	go func() { done <- c.serve(context.Background(), in) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve blocked on an orphan response")
	}
}

func TestConnReadsALineLongerThanBufioDefault(t *testing.T) {
	// An editor pasting a whole buffer as embedded context makes genuinely
	// large frames. bufio's 64KB default would truncate the read and then fail
	// to parse, which looks like a malformed client rather than a short buffer.
	sink := newFrameSink()
	var gotLen int
	c := newConn(sink, func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(params, &p)
		gotLen = len(p.Text)
		return nil, nil
	})
	big := bytes.Repeat([]byte("x"), 300*1024)
	line, err := json.Marshal(rpcMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "big",
		Params:  json.RawMessage(`{"text":"` + string(big) + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.serve(context.Background(), bytes.NewReader(append(line, '\n'))); err != nil {
		t.Fatal(err)
	}
	sink.next(t)
	if gotLen != len(big) {
		t.Errorf("handler saw %d bytes, want %d", gotLen, len(big))
	}
}

func TestConnWritesOneFramePerLine(t *testing.T) {
	// Every frame must be exactly one line: a client reads them with a line
	// scanner, so an embedded newline would split one message into two.
	var buf bytes.Buffer
	c := newConn(&buf, nil)
	c.notify("session/update", sessionNotification{
		SessionID: "s",
		Update:    agentMessage("two\nlines"),
	})
	sc := bufio.NewScanner(&buf)
	n := 0
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("wrote %d lines, want 1", n)
	}
}
