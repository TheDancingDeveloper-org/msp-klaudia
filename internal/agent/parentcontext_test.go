package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
	"github.com/greenthread-ai/klaudia/internal/trust"
)

// recordingApprover counts the approval requests it sees, tagged so a test can
// tell the wiring-time approver from the one the launching turn captured.
type recordingApprover struct {
	name string
	n    atomic.Int32
}

func (a *recordingApprover) Approve(context.Context, ApprovalRequest) permission.Decision {
	a.n.Add(1)
	return permission.Decision{Behavior: permission.Allow}
}

// gatingBash stands in for Bash. It carries the exec permission class, so plan
// mode refuses it, and a command that writes outside the project reaches the
// host gate. It never actually runs anything.
type gatingBash struct{}

func (gatingBash) Name() string                                { return "Bash" }
func (gatingBash) Description(context.Context) (string, error) { return "", nil }
func (gatingBash) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
}
func (gatingBash) ValidateInput(json.RawMessage) error { return nil }
func (gatingBash) PermissionRequest(json.RawMessage) permission.PermissionRequest {
	return permission.PermissionRequest{}
}
func (gatingBash) CheckPermissions(pctx permission.Context, _ permission.PermissionRequest) permission.Decision {
	if permission.CurrentMode(pctx) == permission.ModePlan {
		return permission.Decision{Behavior: permission.Deny, Message: "plan mode is read-only; command execution is not allowed"}
	}
	return permission.Decision{Behavior: permission.Allow}
}
func (gatingBash) Execute(context.Context, tools.Context, json.RawMessage) ([]tools.Result, error) {
	return []tools.Result{{Content: "ok"}}, nil
}

// A child's Bash that changes the machine reaches the approver of the turn that
// launched it, not the one the Spawner was wired with. The wiring-time one never
// saw a /model or a mode change either, which is the same class of staleness.
func TestChildBashReachesTheTurnsApprover(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	roots := trust.NewRoots(filepath.Join(base, "home"), proj)
	gate := &HostGate{Roots: func() trust.Roots { return roots }, Ledger: trust.NewLedger(roots)}

	provider := &paramsProvider{turns: []anthropic.BetaMessage{
		turnFromJSON(t, `{"role":"assistant","stop_reason":"tool_use",
			"content":[{"type":"tool_use","id":"tu1","name":"Bash",
				"input":{"command":"echo x > /etc/klaudia-parentcontext-test"}}]}`),
		turnFromJSON(t, `{"role":"assistant","stop_reason":"end_turn",
			"content":[{"type":"text","text":"done"}]}`),
	}}
	wired := &recordingApprover{name: "wired"}
	turn := &recordingApprover{name: "turn"}
	sp := NewSpawner(provider, tools.NewRegistry(gatingBash{}), "claude-opus-4-8",
		permission.Context{}, wired, 0).WithWorkingDir(proj).WithHostGate(gate)

	if _, err := sp.spawn(context.Background(), ChildSpec{
		Approver: turn,
		Mode:     func() permission.Mode { return permission.ModeAutonomous },
	}, "general-purpose", "do it", nil); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if turn.n.Load() != 1 {
		t.Errorf("turn's approver saw %d requests, want 1", turn.n.Load())
	}
	if wired.n.Load() != 0 {
		t.Errorf("wiring-time approver saw %d requests, want 0", wired.n.Load())
	}
}

// A mode function that flips between the child's turns is read per call, so the
// second Bash is refused once the turn has moved into plan mode.
func TestChildObservesAModeThatFlipsMidRun(t *testing.T) {
	provider := &paramsProvider{turns: []anthropic.BetaMessage{
		turnFromJSON(t, `{"role":"assistant","stop_reason":"tool_use",
			"content":[{"type":"tool_use","id":"tu1","name":"Bash",
				"input":{"command":"echo first"}}]}`),
		turnFromJSON(t, `{"role":"assistant","stop_reason":"tool_use",
			"content":[{"type":"tool_use","id":"tu2","name":"Bash",
				"input":{"command":"echo second"}}]}`),
		turnFromJSON(t, `{"role":"assistant","stop_reason":"end_turn",
			"content":[{"type":"text","text":"done"}]}`),
	}}
	mode := permission.ModeAutonomous
	sp := NewSpawner(provider, tools.NewRegistry(gatingBash{}), "claude-opus-4-8",
		bypassPerm(), nil, 0).WithWorkingDir(t.TempDir())

	out, err := sp.spawn(context.Background(), ChildSpec{Mode: func() permission.Mode {
		m := mode
		mode = permission.ModePlan
		return m
	}}, "general-purpose", "do it", nil)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if len(provider.sent) != 3 {
		t.Fatalf("child took %d turns, want 3", len(provider.sent))
	}
	// The refused call comes back as a tool_result the model is told about.
	raw, _ := json.Marshal(provider.sent[2].Messages)
	if !strings.Contains(string(raw), "plan mode is read-only") {
		t.Errorf("the flipped mode was not observed; result text %q, history %s", out, raw)
	}
}

// The model the launching turn is on — changed by /model after the Spawner was
// wired — is the model the child's requests carry.
func TestChildUsesTheTurnsModel(t *testing.T) {
	provider := &paramsProvider{turns: []anthropic.BetaMessage{
		turnFromJSON(t, `{"role":"assistant","stop_reason":"end_turn",
			"content":[{"type":"text","text":"done"}]}`),
	}}
	sp := NewSpawner(provider, tools.NewRegistry(), "claude-opus-4-8", bypassPerm(), nil, 0)

	if _, err := sp.spawn(context.Background(), ChildSpec{Model: "claude-sonnet-4-6"}, "general-purpose", "do it", nil); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if len(provider.sent) != 1 {
		t.Fatalf("sent %d requests, want 1", len(provider.sent))
	}
	if got := string(provider.sent[0].Model); got != "claude-sonnet-4-6" {
		t.Errorf("child requested model %q, want the turn's", got)
	}
}

// A shared-tree child's Write is checkpointed through the launching turn's
// BeforeEdit, and so are the files an isolated child adopts back.
func TestBeforeEditFiresForSharedAndAdoptedWrites(t *testing.T) {
	t.Run("shared tree", func(t *testing.T) {
		write, err := tools.NewWrite()
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		target := filepath.Join(dir, "a.txt")
		provider := &paramsProvider{turns: []anthropic.BetaMessage{
			turnFromJSON(t, `{"role":"assistant","stop_reason":"tool_use",
				"content":[{"type":"tool_use","id":"tu1","name":"Write","input":{}}]}`),
			turnFromJSON(t, `{"role":"assistant","stop_reason":"end_turn",
				"content":[{"type":"text","text":"done"}]}`),
		}}
		// The path has to be in the tool input: editedPaths reads it from there.
		provider.turns[0] = turnFromJSON(t, `{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Write","input":{"file_path":"`+target+`","content":"hello"}}]}`)

		var got []string
		sp := NewSpawner(provider, tools.NewRegistry(write), "claude-opus-4-8",
			bypassPerm(), nil, 0).WithWorkingDir(dir).WithWorktrees(false)
		if _, err := sp.spawn(context.Background(), ChildSpec{BeforeEdit: func(tool string, paths []string) {
			got = append(got, tool)
			got = append(got, paths...)
		}}, "general-purpose", "do it", nil); err != nil {
			t.Fatalf("spawn: %v", err)
		}
		if len(got) != 2 || got[0] != "Write" || got[1] != target {
			t.Errorf("BeforeEdit saw %v, want [Write %s]", got, target)
		}
	})

	t.Run("adopted files", func(t *testing.T) {
		dir := gitRepo(t)
		w := &relWriteTool{mu: new(sync.Mutex), rel: "b.txt", body: "from child"}
		provider := &paramsProvider{turns: []anthropic.BetaMessage{
			turnFromJSON(t, `{"role":"assistant","stop_reason":"tool_use",
				"content":[{"type":"tool_use","id":"tu1","name":"Write","input":{}}]}`),
			turnFromJSON(t, `{"role":"assistant","stop_reason":"end_turn",
				"content":[{"type":"text","text":"done"}]}`),
		}}
		var gotTool string
		var gotPaths []string
		sp := NewSpawner(provider, tools.NewRegistry(w), "claude-opus-4-8",
			bypassPerm(), nil, 0).WithWorkingDir(dir).WithWorktrees(true)
		if _, err := sp.spawn(context.Background(), ChildSpec{BeforeEdit: func(tool string, paths []string) {
			gotTool = tool
			gotPaths = append(gotPaths, paths...)
		}}, "general-purpose", "do it", nil); err != nil {
			t.Fatalf("spawn: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
			t.Fatalf("adopted file missing: %v", err)
		}
		if gotTool != "Agent" || len(gotPaths) != 1 || filepath.Base(gotPaths[0]) != "b.txt" {
			t.Errorf("BeforeEdit saw %s %v, want Agent [b.txt]", gotTool, gotPaths)
		}
	})
}

// contextProbe records the tools.Context dispatch handed it. Its only job is to
// make the new fields observable.
type contextProbe struct{ seen tools.Context }

func (contextProbe) Name() string                                { return "Probe" }
func (contextProbe) Description(context.Context) (string, error) { return "", nil }
func (contextProbe) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (contextProbe) ValidateInput(json.RawMessage) error { return nil }
func (contextProbe) PermissionRequest(json.RawMessage) permission.PermissionRequest {
	return permission.PermissionRequest{}
}
func (contextProbe) CheckPermissions(permission.Context, permission.PermissionRequest) permission.Decision {
	return permission.Decision{Behavior: permission.Allow}
}
func (p *contextProbe) Execute(_ context.Context, tctx tools.Context, _ json.RawMessage) ([]tools.Result, error) {
	p.seen = tctx
	return []tools.Result{{Content: "ok"}}, nil
}

// The new tools.Context fields are what dispatch copies off the launching turn,
// the same contract TestTurnApplyCopiesEveryField guards for Turn.
func TestDispatchCopiesParentContext(t *testing.T) {
	probe := &contextProbe{}
	loop := New(nil, tools.NewRegistry(probe))

	approver := &recordingApprover{}
	before := func(string, []string) {}
	mode := func() permission.Mode { return permission.ModeAutonomous }
	opts := Options{
		Approver:     approver,
		Permission:   permission.Context{Mode: mode},
		Model:        "claude-sonnet-4-6",
		Effort:       "high",
		Thinking:     "think hard",
		BeforeEdit:   before,
		ExtraDirs:    []string{"/extra"},
		MaxBudgetUSD: 3,
		WorkingDir:   t.TempDir(),
	}
	tu := toolUseBlocks(toolUseTurn(t, "tu1", "Probe", map[string]any{}))[0]
	loop.dispatch(context.Background(), tu, opts, 1, nil, nil, newFailureState())

	c := probe.seen
	if c.Approver != approver {
		t.Errorf("Approver = %v, want the turn's", c.Approver)
	}
	if c.Mode == nil || c.Mode() != permission.ModeAutonomous {
		t.Errorf("Mode was not copied")
	}
	if c.Model != "claude-sonnet-4-6" || c.Effort != "high" || c.Thinking != "think hard" {
		t.Errorf("model settings = %q %q %q", c.Model, c.Effort, c.Thinking)
	}
	if c.BeforeEdit == nil {
		t.Error("BeforeEdit was not copied")
	}
	if len(c.ExtraDirs) != 1 || c.ExtraDirs[0] != "/extra" {
		t.Errorf("ExtraDirs = %v", c.ExtraDirs)
	}
	if c.Budget == nil || *c.Budget != 2 {
		t.Errorf("Budget = %v, want 2 remaining", c.Budget)
	}

	// No budget on the turn means no budget on the child: a pointer to 0 would
	// be "budget spent", which stops the child immediately.
	opts.MaxBudgetUSD = 0
	loop.dispatch(context.Background(), tu, opts, 0, nil, nil, newFailureState())
	if probe.seen.Budget != nil {
		t.Errorf("Budget = %v, want nil when the turn has none", *probe.seen.Budget)
	}
}
