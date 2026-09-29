package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// `--input-format stream-json` is the embedding channel: a persistent agent
// driven over stdin/stdout, one JSON object per line. These scenarios play the
// embedding client — they write user messages and control responses to the
// binary's stdin and react to what it prints — so the protocol is checked in
// the shipped binary, not only in internal/streamjson's unit tests.

// Stream is a running `klaudia --input-format stream-json` process.
type Stream struct {
	t      *testing.T
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser
	lines  chan map[string]any
	seen   []map[string]any // every line read so far, in order
	stderr syncBuffer
}

// syncBuffer is a bytes.Buffer safe to read while exec's copier writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// streamWait bounds each wait for a line. A scripted turn answers in
// milliseconds; this is the ceiling for a regression that hangs.
const streamWait = 20 * time.Second

// StartStream launches klaudia in stream-json input mode with extra args.
func (e *Env) StartStream(extra ...string) *Stream {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	args := append([]string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, extra...)
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = e.Dir
	cmd.Env = e.environ()
	cmd.WaitDelay = 5 * time.Second
	s := &Stream{t: e.t, cmd: cmd, cancel: cancel, lines: make(chan map[string]any, 256)}
	cmd.Stderr = &s.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	s.stdin = stdin
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				s.lines <- m
			}
		}
	}()
	e.t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	return s
}

// Send writes one JSON line to stdin.
func (s *Stream) Send(v any) {
	s.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("writing to klaudia: %v", err)
	}
}

// SendRaw writes a line verbatim, for input that is not valid JSON.
func (s *Stream) SendRaw(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdin, line+"\n"); err != nil {
		s.t.Fatalf("writing to klaudia: %v", err)
	}
}

// SendUser sends a user message; content is a string or content blocks.
func (s *Stream) SendUser(content any) {
	s.Send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
}

// Allow and Deny answer a can_use_tool control_request.
func (s *Stream) Allow(req map[string]any) {
	s.answer(req, map[string]any{"behavior": "allow"})
}

func (s *Stream) Deny(req map[string]any, message string) {
	s.answer(req, map[string]any{"behavior": "deny", "message": message})
}

func (s *Stream) answer(req map[string]any, decision map[string]any) {
	s.Send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": req["request_id"], "response": decision,
	}})
}

// Next returns the next output line of type typ, skipping others.
func (s *Stream) Next(typ string) map[string]any {
	s.t.Helper()
	timeout := time.After(streamWait)
	for {
		select {
		case m, ok := <-s.lines:
			if !ok {
				s.t.Fatalf("klaudia exited before a %q line\nstderr:\n%s", typ, s.stderr.String())
			}
			s.seen = append(s.seen, m)
			if m["type"] == typ {
				return m
			}
		case <-timeout:
			s.t.Fatalf("no %q line within %s\nstderr:\n%s", typ, streamWait, s.stderr.String())
		}
	}
}

// Close ends the session by closing stdin, as an embedder does, drains the
// remaining output and returns the exit code.
func (s *Stream) Close() int {
	s.t.Helper()
	_ = s.stdin.Close()
	for m := range s.lines {
		s.seen = append(s.seen, m)
	}
	err := s.cmd.Wait()
	s.cancel()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	if err != nil {
		s.t.Fatalf("waiting for klaudia: %v", err)
	}
	return 0
}

// Seen returns the lines read so far with the given type.
func (s *Stream) Seen(typ string) []map[string]any {
	var out []map[string]any
	for _, m := range s.seen {
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

// Each user line is a turn with its own result; the conversation carries over
// between turns; every envelope names the session; closing stdin exits 0.
func TestStreamJSONInputMultiTurnConversation(t *testing.T) {
	m := NewFakeModel(t, Say("first answer"), Say("second answer"))
	e := NewEnv(t, m)
	s := e.StartStream()

	s.SendUser("FIRST-QUESTION")
	res1 := s.Next("result")
	if res1["result"] != "first answer" || res1["is_error"] != false || res1["subtype"] != "success" {
		t.Fatalf("first result = %v", res1)
	}
	if usage, _ := res1["usage"].(map[string]any); usage["input_tokens"] != float64(10) {
		t.Errorf("first result usage = %v, want the fake's 10 input tokens", res1["usage"])
	}
	s.SendUser([]any{map[string]any{"type": "text", "text": "SECOND-QUESTION"}})
	if res2 := s.Next("result"); res2["result"] != "second answer" {
		t.Fatalf("second result = %v", res2)
	}
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d after stdin closed\nstderr:\n%s", code, s.stderr.String())
	}

	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model called %d times, want 2", len(reqs))
	}
	if len(reqs[1].Messages) != 3 || !strings.Contains(reqs[1].Raw(), "FIRST-QUESTION") {
		t.Errorf("second turn sent %d messages without the first exchange", len(reqs[1].Messages))
	}

	assistants := s.Seen("assistant")
	if len(assistants) != 2 {
		t.Fatalf("got %d assistant envelopes, want 2", len(assistants))
	}
	sid, _ := assistants[0]["session_id"].(string)
	if sid == "" {
		t.Fatal("assistant envelope has no session_id")
	}
	for _, a := range append(assistants, s.Seen("user")...) {
		if a["session_id"] != sid {
			t.Errorf("envelope session_id %v, want %s", a["session_id"], sid)
		}
	}
}

// A tool the permission mode asks about is put to the client as a
// can_use_tool control_request carrying the tool's input; allowing it runs it.
func TestStreamJSONInputPermissionAllowed(t *testing.T) {
	m := NewFakeModel(t,
		Use("Write", map[string]any{"file_path": "granted.txt", "content": "yes"}),
		Say("written"),
	)
	e := NewEnv(t, m)
	s := e.StartStream("--permission-mode", "default")

	s.SendUser("write the file")
	req := s.Next("control_request")
	body, _ := req["request"].(map[string]any)
	input, _ := body["input"].(map[string]any)
	if body["subtype"] != "can_use_tool" || body["tool_name"] != "Write" || input["file_path"] != "granted.txt" {
		t.Fatalf("control_request = %v", req)
	}
	if _, err := os.Stat(e.Path("granted.txt")); err == nil {
		t.Fatal("the tool ran before the client answered")
	}
	s.Allow(req)
	if res := s.Next("result"); res["result"] != "written" {
		t.Fatalf("result = %v", res)
	}
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, err := os.ReadFile(e.Path("granted.txt")); err != nil || string(got) != "yes" {
		t.Errorf("granted.txt = %q, %v", got, err)
	}
}

// A denial stops the tool, and the client's reason reaches the model as the
// tool's error result.
func TestStreamJSONInputPermissionDenied(t *testing.T) {
	m := NewFakeModel(t,
		Use("Write", map[string]any{"file_path": "refused.txt", "content": "no"}),
		Say("understood"),
	)
	e := NewEnv(t, m)
	s := e.StartStream("--permission-mode", "default")

	s.SendUser("write the file")
	s.Deny(s.Next("control_request"), "CLIENT-SAYS-NO")
	if res := s.Next("result"); res["result"] != "understood" {
		t.Fatalf("result = %v", res)
	}
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(e.Path("refused.txt")); err == nil {
		t.Error("a denied Write ran")
	}
	results := toolResults(s.Seen("user"))
	if len(results) != 1 || results[0]["is_error"] != true || !strings.Contains(resultText(results[0]), "CLIENT-SAYS-NO") {
		t.Errorf("tool results = %v, want one error carrying the client's reason", results)
	}
	if reqs := m.Requests(); len(reqs) != 2 || !strings.Contains(reqs[1].Raw(), "CLIENT-SAYS-NO") {
		t.Error("the denial reason never reached the model")
	}
}

// A client that never answers does not wedge the agent: after --ask-timeout
// the ask is denied, the model is told why, and the turn finishes.
func TestStreamJSONInputUnansweredAskTimesOut(t *testing.T) {
	m := NewFakeModel(t,
		Use("Write", map[string]any{"file_path": "late.txt", "content": "x"}),
		Say("moving on"),
	)
	e := NewEnv(t, m)
	s := e.StartStream("--permission-mode", "default", "--ask-timeout", "200ms")

	s.SendUser("write the file")
	req := s.Next("control_request") // never answered
	if res := s.Next("result"); res["result"] != "moving on" {
		t.Fatalf("result = %v", res)
	}
	// An answer after the timeout is discarded, not applied to a later call.
	s.Allow(req)
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(e.Path("late.txt")); err == nil {
		t.Error("the timed-out Write ran")
	}
	if reqs := m.Requests(); len(reqs) != 2 || !strings.Contains(reqs[1].Raw(), "no control_response arrived within 200ms") {
		t.Error("the model was not told the ask timed out")
	}
}

// A tool on the allow list never reaches the client: an allow rule is the
// client's standing answer.
func TestStreamJSONInputAllowRuleSkipsTheAsk(t *testing.T) {
	m := NewFakeModel(t,
		Use("Write", map[string]any{"file_path": "ruled.txt", "content": "ok"}),
		Say("done"),
	)
	e := NewEnv(t, m)
	s := e.StartStream("--permission-mode", "default", "--allowedTools", "Write", "--ask-timeout", "5s")

	s.SendUser("write the file")
	s.Next("result")
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if n := len(s.Seen("control_request")); n != 0 {
		t.Errorf("%d control_requests for an allow-listed tool", n)
	}
	if got, err := os.ReadFile(e.Path("ruled.txt")); err != nil || string(got) != "ok" {
		t.Errorf("ruled.txt = %q, %v", got, err)
	}
}

// Lines that are not a user message with text — malformed JSON, other types,
// empty content — are skipped without ending the session or calling the model.
func TestStreamJSONInputSkipsLinesThatAreNotPrompts(t *testing.T) {
	m := NewFakeModel(t, Say("only this"))
	e := NewEnv(t, m)
	s := e.StartStream()

	s.SendRaw("this is not json")
	s.Send(map[string]any{"type": "keep_alive"})
	s.SendUser("")
	s.SendUser([]any{map[string]any{"type": "image", "source": map[string]any{}}})
	s.Send(map[string]any{"type": "control_response", "response": map[string]any{"request_id": "unknown"}})
	s.SendUser("REAL-PROMPT")
	if res := s.Next("result"); res["result"] != "only this" {
		t.Fatalf("result = %v", res)
	}
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, s.stderr.String())
	}
	reqs := m.Requests()
	if len(reqs) != 1 || len(reqs[0].Messages) != 1 || !strings.Contains(reqs[0].Raw(), "REAL-PROMPT") {
		t.Errorf("requests = %d, want exactly the one real prompt", len(reqs))
	}
	if n := len(s.Seen("result")); n != 1 {
		t.Errorf("%d result lines, want 1", n)
	}
}

// An API failure is reported on that turn's result line; the session stays up
// for the next message.
func TestStreamJSONInputAPIErrorEndsTurnNotSession(t *testing.T) {
	m := NewFakeModel(t, Fail(400, "invalid_request_error"), Say("recovered"))
	e := NewEnv(t, m)
	s := e.StartStream()

	s.SendUser("first")
	res := s.Next("result")
	if res["is_error"] != true || res["subtype"] != "error_during_execution" ||
		!strings.HasPrefix(res["result"].(string), "Error: ") {
		t.Fatalf("failed turn result = %v", res)
	}
	s.SendUser("second")
	if res := s.Next("result"); res["result"] != "recovered" || res["is_error"] != false {
		t.Fatalf("next turn result = %v", res)
	}
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
