package streamjson

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/permission"
)

// session drives a Driver over a pipe for the control-request tests.
type session struct {
	t    *testing.T
	d    *Driver
	out  *lineSink
	pw   *io.PipeWriter
	done chan error
}

func startSession(t *testing.T, d *Driver, out *lineSink, run RunFunc) *session {
	t.Helper()
	pr, pw := io.Pipe()
	s := &session{t: t, d: d, out: out, pw: pw, done: make(chan error, 1)}
	go func() { s.done <- d.Run(context.Background(), pr, run) }()
	t.Cleanup(func() { _ = pw.Close() })
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.pw, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) user(text string) {
	s.send(`{"type":"user","message":{"role":"user","content":"` + text + `"}}`)
}

func (s *session) control(id, request string) {
	s.send(`{"type":"control_request","request_id":"` + id + `","request":` + request + `}`)
}

// finish closes stdin and waits for Run to return.
func (s *session) finish() {
	s.t.Helper()
	_ = s.pw.Close()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("driver run: %v", err)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatalf("driver did not return after stdin closed:\n%s", strings.Join(s.out.snapshot(), "\n"))
	}
}

// waitLines polls until pred holds for at least n output lines and returns them.
func (s *session) waitLines(n int, pred func(map[string]any) bool) []map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got []map[string]any
		for _, l := range s.out.snapshot() {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil && pred(m) {
				got = append(got, m)
			}
		}
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %d matching lines:\n%s", n, strings.Join(s.out.snapshot(), "\n"))
	return nil
}

// response waits for the control_response to request id and returns its body.
func (s *session) response(id string) map[string]any {
	s.t.Helper()
	return s.waitLines(1, func(m map[string]any) bool {
		r, _ := m["response"].(map[string]any)
		return m["type"] == "control_response" && r != nil && r["request_id"] == id
	})[0]["response"].(map[string]any)
}

func isResult(m map[string]any) bool { return m["type"] == "result" }

// blockUntilCancelled is a turn that runs until its context is cancelled.
func blockUntilCancelled(started chan<- string) RunFunc {
	return func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
		prompt := turn.Prompt
		if started != nil {
			started <- prompt
		}
		<-ctx.Done()
		return agent.Result{NumTurns: 1}, ctx.Err()
	}
}

// An interrupt cancels the running turn — before this the only way an embedder
// could stop a turn was to kill the process — and is answered with a success
// control_response carrying the same request_id. The turn still ends with a
// result line, marked as interrupted rather than as a bare context error.
func TestInterruptCancelsTheRunningTurn(t *testing.T) {
	out := &lineSink{}
	d := NewDriver(out)
	d.SessionID = "sess-int"
	started := make(chan string, 1)
	s := startSession(t, d, out, blockUntilCancelled(started))

	s.user("long job")
	<-started
	s.control("int-1", `{"subtype":"interrupt"}`)

	if r := s.response("int-1"); r["subtype"] != "success" {
		t.Fatalf("interrupt response = %v, want success", r)
	}
	res := s.waitLines(1, isResult)[0]
	if res["subtype"] != "error_during_execution" || res["is_error"] != true ||
		!strings.Contains(res["result"].(string), "Interrupted") {
		t.Errorf("interrupted result = %v", res)
	}
	if res["session_id"] != "sess-int" {
		t.Errorf("result session_id = %v, want sess-int", res["session_id"])
	}
	s.finish()
}

// Cancelling the turn releases a can_use_tool ask it is parked on, as Esc does
// in the TUI; the ask does not sit out the rest of --ask-timeout.
func TestInterruptReleasesAPendingAsk(t *testing.T) {
	out := &lineSink{}
	d := NewDriver(out)
	decided := make(chan permission.Decision, 1)
	s := startSession(t, d, out, func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
		ap := turn.Approver
		dec := ap.Approve(ctx, agent.ApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)})
		decided <- dec
		return agent.Result{}, ctx.Err()
	})

	s.user("ask")
	if waitForRequestID(out) == "" {
		t.Fatal("no can_use_tool request")
	}
	s.control("int-2", `{"subtype":"interrupt"}`)
	select {
	case dec := <-decided:
		if dec.Behavior != permission.Deny {
			t.Errorf("ask resolved to %v after interrupt, want deny", dec.Behavior)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ask was not released by the interrupt")
	}
	s.waitLines(1, isResult)
	s.finish()
}

// An interrupt reaches turns the peer sent before it that have not started:
// each gets an interrupted result line without running. A turn sent after the
// interrupt runs normally, as does one after an interrupt sent while idle.
func TestInterruptCoversQueuedTurnsOnly(t *testing.T) {
	out := &lineSink{}
	d := NewDriver(out)
	var ran []string
	started := make(chan string, 4)
	s := startSession(t, d, out, func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
		prompt := turn.Prompt
		ran = append(ran, prompt)
		if prompt == "first" {
			started <- prompt
			<-ctx.Done()
			return agent.Result{}, ctx.Err()
		}
		return agent.Result{Text: "ran:" + prompt}, nil
	})

	s.user("first")
	<-started
	s.user("second") // queued behind the running turn
	// The reader handles lines in order, so "second" is read before this.
	s.control("int-3", `{"subtype":"interrupt"}`)
	s.response("int-3")
	s.waitLines(2, isResult)
	s.user("third")
	s.waitLines(3, isResult)
	s.control("int-4", `{"subtype":"interrupt"}`) // idle: nothing to cancel
	s.response("int-4")
	s.user("fourth")
	results := s.waitLines(4, isResult)
	s.finish()

	if got := strings.Join(ran, ","); got != "first,third,fourth" {
		t.Errorf("turns run = %s, want first,third,fourth (second was interrupted before it started)", got)
	}
	for i, want := range []string{"Interrupted", "Interrupted", "ran:third", "ran:fourth"} {
		if r, _ := results[i]["result"].(string); !strings.Contains(r, want) {
			t.Errorf("result %d = %q, want it to contain %q", i, r, want)
		}
	}
}

// The reader never blocks on queued turns. It used to hand user lines to the
// turn loop over an 8-slot channel, so a peer that sent ahead while a turn
// waited on a can_use_tool answer filled the channel, stalled the reader, and
// the answer behind those lines was never read: a deadlock. The same stall
// would have hidden an interrupt.
func TestReaderKeepsReadingWhileTurnsQueue(t *testing.T) {
	out := &lineSink{}
	d := NewDriver(out)
	var calls atomic.Int32
	s := startSession(t, d, out, func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
		ap := turn.Approver
		if calls.Add(1) == 1 {
			dec := ap.Approve(ctx, agent.ApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{}`)})
			return agent.Result{Text: "asked:" + string(dec.Behavior)}, nil
		}
		return agent.Result{Text: "queued"}, nil
	})

	s.user("asks")
	id := waitForRequestID(out)
	for i := 0; i < 20; i++ {
		s.user(fmt.Sprintf("ahead-%d", i))
	}
	s.send(`{"type":"control_response","response":{"subtype":"success","request_id":"` + id + `","response":{"behavior":"allow"}}}`)
	results := s.waitLines(21, isResult)
	s.finish()
	if results[0]["result"] != "asked:allow" {
		t.Errorf("first turn = %v, want asked:allow", results[0]["result"])
	}
}

// set_permission_mode and set_model are handed to the session's handlers; a
// handler's error comes back as an error control_response, and a session that
// wired no handler says so rather than pretending to comply.
func TestSetPermissionModeAndSetModel(t *testing.T) {
	out := &lineSink{}
	d := NewDriver(out)
	var modes, models []string
	d.SetPermissionMode = func(m string) error {
		if m == "bypassPermissions" {
			return errors.New("not launched with it")
		}
		modes = append(modes, m)
		return nil
	}
	d.SetModel = func(m string) (string, error) {
		models = append(models, m)
		if m == "" {
			return "launch-model", nil
		}
		return "resolved-" + m, nil
	}
	s := startSession(t, d, out, blockUntilCancelled(nil))

	s.control("m1", `{"subtype":"set_permission_mode","mode":"plan"}`)
	if r := s.response("m1"); r["subtype"] != "success" || r["response"].(map[string]any)["mode"] != "plan" {
		t.Errorf("set_permission_mode plan = %v", r)
	}
	s.control("m2", `{"subtype":"set_permission_mode","mode":"bypassPermissions"}`)
	if r := s.response("m2"); r["subtype"] != "error" || !strings.Contains(r["error"].(string), "not launched") {
		t.Errorf("refused set_permission_mode = %v, want error carrying the reason", r)
	}
	s.control("s1", `{"subtype":"set_model","model":"opus"}`)
	if r := s.response("s1"); r["subtype"] != "success" || r["response"].(map[string]any)["model"] != "resolved-opus" {
		t.Errorf("set_model opus = %v", r)
	}
	s.control("s2", `{"subtype":"set_model"}`)
	s.response("s2")
	s.control("s3", `{"subtype":"set_model","model":"default"}`)
	if r := s.response("s3"); r["response"].(map[string]any)["model"] != "launch-model" {
		t.Errorf("set_model default = %v, want the launch model", r)
	}
	s.finish()

	if strings.Join(modes, ",") != "plan" {
		t.Errorf("modes applied = %v, want [plan]", modes)
	}
	if strings.Join(models, "|") != "opus||" {
		t.Errorf("models requested = %q, want opus then two resets to the default", models)
	}

	// No handlers wired.
	out2 := &lineSink{}
	s2 := startSession(t, NewDriver(out2), out2, blockUntilCancelled(nil))
	s2.control("x1", `{"subtype":"set_permission_mode","mode":"plan"}`)
	s2.control("x2", `{"subtype":"set_model","model":"opus"}`)
	for _, id := range []string{"x1", "x2"} {
		if r := s2.response(id); r["subtype"] != "error" {
			t.Errorf("%s without a handler = %v, want error", id, r)
		}
	}
	s2.finish()
}

// initialize is answered once, with Claude Code's reply fields; a second one is
// an error, and so is one asking for callbacks Klaudia cannot honour (a hook
// that is silently never run is worse than a refused handshake). Options the
// SDK sends as null are not requests for anything.
func TestInitializeHandshake(t *testing.T) {
	out := &lineSink{}
	s := startSession(t, NewDriver(out), out, blockUntilCancelled(nil))

	s.control("h1", `{"subtype":"initialize","hooks":{"PreToolUse":[{"matcher":"Bash","hookCallbackIds":["h"]}]}}`)
	if r := s.response("h1"); r["subtype"] != "error" || !strings.Contains(r["error"].(string), "hooks") {
		t.Errorf("initialize with hooks = %v, want an error naming hooks", r)
	}
	s.control("i1", `{"subtype":"initialize","hooks":null,"systemPrompt":""}`)
	r := s.response("i1")
	if r["subtype"] != "success" {
		t.Fatalf("initialize = %v, want success", r)
	}
	body := r["response"].(map[string]any)
	for _, k := range []string{"commands", "models", "output_style", "available_output_styles"} {
		if _, ok := body[k]; !ok {
			t.Errorf("initialize response has no %q: %v", k, body)
		}
	}
	s.control("i2", `{"subtype":"initialize"}`)
	if r := s.response("i2"); r["subtype"] != "error" {
		t.Errorf("second initialize = %v, want error", r)
	}
	s.control("u1", `{"subtype":"rewind_files"}`)
	if r := s.response("u1"); r["subtype"] != "error" || !strings.Contains(r["error"].(string), "rewind_files") {
		t.Errorf("unknown subtype = %v, want an error naming it", r)
	}
	s.finish()
}

// The result line carries session_id and duration_ms, as the -p result does,
// and no total_cost_usd: Klaudia does not price turns yet, and a placeholder 0
// would read as a free turn.
func TestResultLineCarriesSessionAndDuration(t *testing.T) {
	out := &lineSink{}
	d := NewDriver(out)
	d.SessionID = "sess-r"
	in := strings.NewReader(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n")
	err := d.Run(context.Background(), in, func(context.Context, agent.Turn) (agent.Result, error) {
		time.Sleep(20 * time.Millisecond)
		return agent.Result{Text: "ok"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	for _, l := range out.snapshot() {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && isResult(m) {
			res = m
		}
	}
	if res == nil {
		t.Fatal("no result line")
	}
	if res["session_id"] != "sess-r" {
		t.Errorf("session_id = %v", res["session_id"])
	}
	if ms, ok := res["duration_ms"].(float64); !ok || ms < 20 {
		t.Errorf("duration_ms = %v, want >= 20", res["duration_ms"])
	}
	if c, ok := res["total_cost_usd"].(float64); !ok || c != 0 {
		t.Errorf("total_cost_usd = %v, want 0 for a run with no priced usage", res["total_cost_usd"])
	}
}
