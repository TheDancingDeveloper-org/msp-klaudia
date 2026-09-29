package mcp

import (
	"context"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A server that changes its tool list after connect sends
// notifications/tools/list_changed. Klaudia must react by re-listing the
// server live and firing the registered rebuild handler, so a tool that appears
// mid-session becomes usable without a config edit or restart.
func TestToolsListChangedRefreshesRegistry(t *testing.T) {
	ctx := context.Background()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "live", Version: "0.0.1"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "first", Description: "the original tool"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ echoIn) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{}, nil, nil
		})

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}

	m := &Manager{}
	// Connect through the manager's client options so the tools/list_changed
	// handler is installed on the session, exactly as Connect/Reconnect do.
	client, err := connectTransport(ctx, "live", clientT, m.clientOptions())
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	m.Add(client)
	defer func() { m.Close(); _ = ss.Wait() }()

	fired := make(chan struct{}, 4)
	m.SetToolsChangedHandler(func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	if got := len(m.Tools(ctx)); got != 1 {
		t.Fatalf("before change: %d tools, want 1", got)
	}

	// Add a tool after connect → the server emits tools/list_changed.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "second", Description: "added mid-session"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ echoIn) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{}, nil, nil
		})

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("tools/list_changed handler was not called")
	}

	// The live re-list now sees both tools.
	names := map[string]bool{}
	for _, tool := range m.Tools(ctx) {
		names[tool.Name()] = true
	}
	if !names["mcp__live__first"] || !names["mcp__live__second"] {
		t.Errorf("after change: tools = %v, want both first and second", names)
	}
}

// With no handler registered, a list_changed notification must be a harmless
// no-op rather than a nil-call panic.
func TestToolsListChangedWithoutHandlerIsSafe(t *testing.T) {
	m := &Manager{}
	m.dispatchToolsChanged() // must not panic
}
