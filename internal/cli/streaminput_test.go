package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lineChan is an io.Writer that turns each written JSON line into a decoded
// message on a channel, so a test can react to output while the run is live.
type lineChan struct {
	mu  sync.Mutex
	buf bytes.Buffer
	ch  chan map[string]any
}

func newLineChan() *lineChan { return &lineChan{ch: make(chan map[string]any, 256)} }

func (l *lineChan) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadBytes('\n')
		if err != nil {
			// Incomplete line: put it back for the next write.
			rest := append([]byte(nil), line...)
			l.buf.Reset()
			l.buf.Write(rest)
			return len(p), nil
		}
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			l.ch <- m
		}
	}
}

// next returns the next line of the given type, failing after a timeout.
func (l *lineChan) next(t *testing.T, typ string) map[string]any {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m := <-l.ch:
			if m["type"] == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("no %q line within 20s", typ)
			return nil
		}
	}
}

// streamSession runs `--input-format stream-json` in-process with a live stdin.
type streamSession struct {
	t     *testing.T
	stdin *io.PipeWriter
	out   *lineChan
	done  chan error
	errb  *bytes.Buffer
}

func startStream(t *testing.T, e *cliEnv, extra ...string) *streamSession {
	t.Helper()
	pr, pw := io.Pipe()
	s := &streamSession{t: t, stdin: pw, out: newLineChan(), done: make(chan error, 1), errb: &bytes.Buffer{}}
	args := append([]string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, extra...)
	go func() { s.done <- e.runTo(pr, s.out, s.errb, args...) }()
	t.Cleanup(func() { _ = pw.Close() })
	return s
}

func (s *streamSession) send(v any) {
	s.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

func (s *streamSession) sendUser(content any) {
	s.send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
}

// finish closes stdin and waits for the command to return.
func (s *streamSession) finish() error {
	s.t.Helper()
	_ = s.stdin.Close()
	select {
	case err := <-s.done:
		return err
	case <-time.After(20 * time.Second):
		s.t.Fatal("stream-json run did not end after stdin closed")
		return nil
	}
}

// Each user line is a turn, and the conversation carries across turns; closing
// stdin ends the session cleanly.
func TestRunStreamJSONInputCarriesHistoryAcrossTurns(t *testing.T) {
	m := newFakeModel(t, say("first reply"), say("second reply"))
	e := newCLIEnv(t, m)
	s := startStream(t, e)

	s.sendUser("FIRST-PROMPT")
	if res := s.out.next(t, "result"); res["result"] != "first reply" || res["is_error"] != false {
		t.Fatalf("first result = %v", res)
	}
	s.sendUser([]any{map[string]any{"type": "text", "text": "SECOND-PROMPT"}})
	if res := s.out.next(t, "result"); res["result"] != "second reply" {
		t.Fatalf("second result = %v", res)
	}
	if err := s.finish(); err != nil {
		t.Fatalf("run returned %v\n%s", err, s.errb)
	}
	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model called %d times, want 2", len(reqs))
	}
	if len(reqs[1].Messages) != 3 || !strings.Contains(reqs[1].Raw(), "FIRST-PROMPT") {
		t.Errorf("second turn did not carry the first: %d messages", len(reqs[1].Messages))
	}
}

// A tool the permission flow cannot settle is put to the peer as a
// can_use_tool control_request; the peer's allow lets it run.
func TestRunStreamJSONInputAsksPeerAndHonoursAllow(t *testing.T) {
	m := newFakeModel(t,
		use("Write", map[string]any{"file_path": "asked.txt", "content": "allowed"}),
		say("wrote"),
	)
	e := newCLIEnv(t, m)
	s := startStream(t, e, "--permission-mode", "default")

	s.sendUser("write it")
	req := s.out.next(t, "control_request")
	body, _ := req["request"].(map[string]any)
	if body["subtype"] != "can_use_tool" || body["tool_name"] != "Write" {
		t.Fatalf("control_request = %v", req)
	}
	s.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": req["request_id"],
		"response": map[string]any{"behavior": "allow"},
	}})
	if res := s.out.next(t, "result"); res["result"] != "wrote" {
		t.Fatalf("result = %v", res)
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(e.Dir, "asked.txt")); err != nil || string(got) != "allowed" {
		t.Errorf("asked.txt = %q, %v", got, err)
	}
}

// --ask-timeout bounds the wait: an unanswered ask is denied and the turn
// still finishes.
func TestRunStreamJSONInputAskTimeoutDenies(t *testing.T) {
	m := newFakeModel(t,
		use("Write", map[string]any{"file_path": "never.txt", "content": "x"}),
		say("gave up"),
	)
	e := newCLIEnv(t, m)
	s := startStream(t, e, "--permission-mode", "default", "--ask-timeout", "100ms")

	s.sendUser("write it")
	s.out.next(t, "control_request") // deliberately unanswered
	if res := s.out.next(t, "result"); res["result"] != "gave up" {
		t.Fatalf("result = %v", res)
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "never.txt")); err == nil {
		t.Error("an unanswered ask was treated as allow")
	}
	if reqs := m.Requests(); len(reqs) != 2 || !strings.Contains(reqs[1].Raw(), "no control_response arrived within 100ms") {
		t.Error("the model was not told why the tool was denied")
	}
}
