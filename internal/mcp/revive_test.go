package mcp

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/greenthread-ai/klaudia/internal/tools"
)

// reviveHarness is a manager whose server can be killed and whose relaunches
// are counted, with an in-memory peer standing in for a child process.
type reviveHarness struct {
	m        *Manager
	srv      *Server
	launches *atomic.Int32
	calls    *atomic.Int32
	peer     func() *mcpsdk.ServerSession
}

// newReviveHarness wires a live server plus a connect function that builds a
// fresh in-memory peer each time, which is what a relaunched child process
// amounts to from the client's side.
func newReviveHarness(t *testing.T, readOnly bool) *reviveHarness {
	t.Helper()
	var launches, calls atomic.Int32
	var mu sync.Mutex
	var peers []*mcpsdk.ServerSession

	spawn := func(name string) (*Server, error) {
		srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "testsrv", Version: "0.0.1"}, nil)
		mcpsdk.AddTool(srv, &mcpsdk.Tool{
			Name:        "echo",
			Description: "Echo the message back",
			Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: readOnly},
		},
			func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, any, error) {
				calls.Add(1)
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + in.Message}},
				}, nil, nil
			})
		clientT, serverT := mcpsdk.NewInMemoryTransports()
		ss, err := srv.Connect(context.Background(), serverT, nil)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		peers = append(peers, ss)
		mu.Unlock()
		return ConnectTransport(context.Background(), name, clientT, nil)
	}

	first, err := spawn("testsrv")
	if err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	m := &Manager{
		ctx: context.Background(),
		cfg: Config{MCPServers: map[string]ServerConfig{"testsrv": {Command: "testsrv"}}},
		connect: func(_ context.Context, name string, _ ServerConfig, _ *Elicitor) (*Server, error) {
			launches.Add(1)
			return spawn(name)
		},
	}
	m.Add(first)
	t.Cleanup(func() { m.Close() })

	return &reviveHarness{
		m: m, srv: first, launches: &launches, calls: &calls,
		peer: func() *mcpsdk.ServerSession {
			mu.Lock()
			defer mu.Unlock()
			return peers[len(peers)-1]
		},
	}
}

func (h *reviveHarness) killPeer(t *testing.T) {
	t.Helper()
	if err := h.peer().Close(); err != nil {
		t.Fatalf("closing the peer: %v", err)
	}
	// The precondition that makes the bug possible: the client still believes
	// it holds a session, so nothing short of a probe can tell.
	if !h.srv.Connected() {
		t.Fatal("precondition failed: the session went nil on peer death")
	}
}

// echoTool resolves the wrapper once, which is how a real session works: the
// registry is built at startup and the wrappers are reused for the rest of it.
// Re-listing instead would hide the bug, because Manager.Tools skips a server
// whose session no longer answers.
func (h *reviveHarness) echoTool(t *testing.T, ctx context.Context) tools.Tool {
	t.Helper()
	for _, tool := range h.m.Tools(ctx) {
		if strings.HasSuffix(tool.Name(), "__echo") {
			return tool
		}
	}
	t.Fatal("echo tool not found")
	return nil
}

func callEcho(t *testing.T, ctx context.Context, tool tools.Tool) string {
	t.Helper()
	res, err := tool.Execute(ctx, tools.Context{}, []byte(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("no result")
	}
	return res[0].Content
}

// A reload notices a dead server, but only when the config changes. A server
// that dies mid-session with nobody editing .mcp.json stayed dead for the rest
// of the session: every call failed, and the cure was /mcp or a restart.
func TestAReadOnlyToolCallRevivesADeadServer(t *testing.T) {
	h := newReviveHarness(t, true)
	ctx := context.Background()

	echo := h.echoTool(t, ctx)
	if got := callEcho(t, ctx, echo); got != "echo: hi" {
		t.Fatalf("precondition: echo returned %q", got)
	}
	h.killPeer(t)

	if got := callEcho(t, ctx, echo); got != "echo: hi" {
		t.Errorf("call after the server died returned %q, want it to work after a relaunch", got)
	}
	if n := h.launches.Load(); n != 1 {
		t.Errorf("%d relaunches, want 1", n)
	}
}

// Whether to repeat the call is a correctness question, not a performance one.
// The failure may have happened on the way back from a call that already ran,
// and nothing on the wire distinguishes that from one that never arrived — so a
// tool that is not declared read-only is reported with the ambiguity stated
// rather than silently run twice.
func TestAMutatingToolCallIsNotRepeatedAfterARevive(t *testing.T) {
	h := newReviveHarness(t, false)
	ctx := context.Background()

	echo := h.echoTool(t, ctx)
	if got := callEcho(t, ctx, echo); got != "echo: hi" {
		t.Fatalf("precondition: echo returned %q", got)
	}
	before := h.calls.Load()
	h.killPeer(t)

	got := callEcho(t, ctx, echo)
	if !strings.Contains(got, "restarted") {
		t.Errorf("result = %q, want it to say the server was restarted", got)
	}
	if !strings.Contains(got, "may or may not") {
		t.Errorf("result = %q, want it to state that the call's effect is unknown", got)
	}
	if n := h.calls.Load(); n != before {
		t.Errorf("the call ran again on the new server: %d calls, want %d", n, before)
	}
	// The server is nonetheless usable again, which is the point of reviving
	// it even when the call cannot be retried.
	if !h.srv.alive(ctx) {
		t.Error("the server was not revived")
	}
}

// A bad argument and an unknown tool also arrive as call errors. Restarting a
// healthy server because the model made a mistake would throw away whatever
// state that server holds — for an editor bridge, the user's open session.
func TestACallErrorDoesNotRestartAHealthyServer(t *testing.T) {
	h := newReviveHarness(t, true)
	ctx := context.Background()

	tl := h.m.Tools(ctx)
	if len(tl) == 0 {
		t.Fatal("no tools")
	}
	// A tool name the server does not have, reached through a wrapper that is
	// otherwise correctly wired.
	bad := &mcpTool{qualifiedName: "mcp__testsrv__nope", remoteName: "nope", server: h.srv, mgr: h.m, readOnly: true}
	res, err := bad.Execute(ctx, tools.Context{}, []byte(`{}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res) == 0 || !res[0].IsError {
		t.Fatalf("expected an error result, got %+v", res)
	}
	if n := h.launches.Load(); n != 0 {
		t.Errorf("a healthy server was relaunched %d times over a call error", n)
	}
}

// A server that cannot start — a bad command, a missing binary — would
// otherwise pay a full launch timeout on every call the model makes, turning
// one misconfigured entry into a stalled session.
func TestReviveIsRateLimited(t *testing.T) {
	h := newReviveHarness(t, true)
	ctx := context.Background()
	h.m.mu.Lock()
	h.m.connect = func(context.Context, string, ServerConfig, *Elicitor) (*Server, error) {
		h.launches.Add(1)
		return nil, context.DeadlineExceeded
	}
	h.m.mu.Unlock()

	h.killPeer(t)
	for i := 0; i < 3; i++ {
		if !h.m.revive(ctx, h.srv) {
			continue
		}
		t.Fatalf("attempt %d reported success with a connect that always fails", i)
	}
	if n := h.launches.Load(); n != 1 {
		t.Errorf("%d launch attempts, want 1 inside the cooldown", n)
	}
}

// Two tool calls, or a tool call and /mcp, can arrive together. Without the
// per-server lock each would spawn a child process and close the other's.
func TestConcurrentRevivesLaunchOneServer(t *testing.T) {
	h := newReviveHarness(t, true)
	ctx := context.Background()
	h.killPeer(t)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.m.revive(ctx, h.srv)
		}()
	}
	wg.Wait()

	if n := h.launches.Load(); n != 1 {
		t.Errorf("%d concurrent relaunches, want 1", n)
	}
	if !h.srv.alive(ctx) {
		t.Error("the server is not usable after concurrent revives")
	}
}

// A revive must not be attempted at all while the server is answering: the
// probe, not the caller, decides.
func TestReviveLeavesALiveServerAlone(t *testing.T) {
	h := newReviveHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !h.m.revive(ctx, h.srv) {
		t.Fatal("revive reported failure for a live server")
	}
	if n := h.launches.Load(); n != 0 {
		t.Errorf("a live server was relaunched %d times", n)
	}
}
