package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// seen records the tool names a child was handed.
type seen struct {
	mu    sync.Mutex
	names [][]string
}

func (s *seen) add(names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, names)
}

// listingTool records the registry it was called with, which is the child's.
type listingTool struct{ seen *seen }

func (listingTool) Name() string                                { return "Look" }
func (listingTool) Description(context.Context) (string, error) { return "", nil }
func (listingTool) InputSchema() json.RawMessage                { return json.RawMessage(`{"type":"object"}`) }
func (listingTool) ValidateInput(json.RawMessage) error         { return nil }
func (listingTool) PermissionRequest(json.RawMessage) permission.PermissionRequest {
	return permission.PermissionRequest{}
}
func (listingTool) CheckPermissions(permission.Context, permission.PermissionRequest) permission.Decision {
	return permission.Decision{Behavior: permission.Allow}
}
func (l listingTool) Execute(_ context.Context, tctx tools.Context, _ json.RawMessage) ([]tools.Result, error) {
	var names []string
	if tctx.Registry != nil {
		names = tctx.Registry.Names()
	}
	l.seen.add(names)
	return []tools.Result{{Content: "ok"}}, nil
}

// stubAgent stands in for the Agent tool. The depth test only cares that it is
// present or absent in the child's registry, so it never runs.
type stubAgent struct{}

func (stubAgent) Name() string                                { return "Agent" }
func (stubAgent) Description(context.Context) (string, error) { return "", nil }
func (stubAgent) InputSchema() json.RawMessage                { return json.RawMessage(`{"type":"object"}`) }
func (stubAgent) ValidateInput(json.RawMessage) error         { return nil }
func (stubAgent) PermissionRequest(json.RawMessage) permission.PermissionRequest {
	return permission.PermissionRequest{}
}
func (stubAgent) CheckPermissions(permission.Context, permission.PermissionRequest) permission.Decision {
	return permission.Decision{Behavior: permission.Allow}
}
func (stubAgent) Execute(context.Context, tools.Context, json.RawMessage) ([]tools.Result, error) {
	return []tools.Result{{Content: "ok"}}, nil
}

// max_depth bounds how far a child may launch another. At 2 the first child
// has the Agent tool and the one it launches does not, so there is no third
// level. At the default of 1 no child has it.
func TestMaxDepthCapsNestedAgents(t *testing.T) {
	got := &seen{}
	base := tools.NewRegistry(listingTool{got})
	spawn := func(depth, cap int) {
		t.Helper()
		s := NewSpawner(&scriptedProvider{turns: []anthropic.BetaMessage{
			toolUseTurn(t, "tu1", "Look", map[string]any{}),
		}}, base, "claude-opus-4-8", bypassPerm(), nil, 2).WithAgentTool(stubAgent{}, cap)
		if _, _, err := s.spawn(context.Background(), ChildSpec{Depth: depth}, "general-purpose", "go", nil); err != nil {
			t.Fatal(err)
		}
	}
	spawn(0, 2)
	spawn(1, 2)
	got.mu.Lock()
	names := append([][]string(nil), got.names...)
	got.mu.Unlock()
	if len(names) != 2 {
		t.Fatalf("saw %d children, want 2", len(names))
	}
	if !strings.Contains(strings.Join(names[0], ","), "Agent") {
		t.Errorf("a child at depth 0 with max_depth 2 was not given the Agent tool: %v", names[0])
	}
	if strings.Contains(strings.Join(names[1], ","), "Agent") {
		t.Errorf("a child at depth 1 was given the Agent tool, so a third level is possible: %v", names[1])
	}
	spawn(0, 1)
	got.mu.Lock()
	defer got.mu.Unlock()
	if strings.Contains(strings.Join(got.names[2], ","), "Agent") {
		t.Errorf("the default depth gave a child the Agent tool: %v", got.names[2])
	}
}
