package agent

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/subagent"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// pricedProvider returns one priced reply and then ends the turn, so a child's
// cost is a known quantity.
type pricedProvider struct {
	out   int64
	calls int
}

func (p *pricedProvider) StreamTurn(_ context.Context, _ anthropic.BetaMessageNewParams, _ api.StreamSink) (anthropic.BetaMessage, error) {
	p.calls++
	if p.calls > 1 {
		return anthropic.BetaMessage{StopReason: "end_turn"}, nil
	}
	return anthropic.BetaMessage{
		StopReason: "end_turn",
		Usage:      anthropic.BetaUsage{OutputTokens: p.out},
		Content:    []anthropic.BetaContentBlockUnion{{Type: "text", Text: "done"}},
	}, nil
}

// agentTool wraps the spawner as the Agent tool. The tool's spawner field is
// unexported and there is no setter, so the test builds it the way the CLI does.
func agentRegistry(t *testing.T, child api.Provider) *tools.Registry {
	t.Helper()
	sp := NewSpawner(child, tools.NewRegistry(), anthropic.Model("claude-sonnet-5"), permission.Context{}, nil, 0).
		WithTypes(subagent.Builtin())
	a, err := tools.NewAgent(sp, []tools.AgentTypeInfo{{Name: "Explore", Description: "look"}})
	if err != nil {
		t.Fatal(err)
	}
	return tools.NewRegistry(a)
}

// The parent's cost includes what its child spent, and the breakdown names it.
func TestParentCostIncludesChild(t *testing.T) {
	child := &pricedProvider{out: 1_000_000}
	parent := &scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu_agent", "Agent", map[string]any{
			"subagent_type": "Explore", "prompt": "look", "description": "look",
		}),
	}}
	res, err := New(parent, agentRegistry(t, child)).Run(context.Background(), Options{
		Prompt: "delegate",
		Model:  "claude-sonnet-5",
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, _ := api.CostUSD("claude-sonnet-5", api.Usage{OutputTokens: 1_000_000})
	if res.CostUSD != want {
		t.Errorf("parent CostUSD = %v, want the child's %v; children=%+v childCalls=%d", res.CostUSD, want, res.Children, child.calls)
	}
	if len(res.Children) != 1 || res.Children[0].CostUSD != want || res.Children[0].Model != "claude-sonnet-5" {
		t.Errorf("children = %+v, want one entry at %v", res.Children, want)
	}
}

// A budget the parent's own requests stay under is still enforced once the
// child's spend pushes the total over it: the parent is not asked again.
func TestChildSpendTripsParentBudget(t *testing.T) {
	child := &pricedProvider{out: 1_000_000}
	parent := &scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu_agent", "Agent", map[string]any{
			"subagent_type": "Explore", "prompt": "look", "description": "look",
		}),
		toolUseTurn(t, "tu_agent_2", "Agent", map[string]any{
			"subagent_type": "Explore", "prompt": "look again", "description": "again",
		}),
	}}
	res, err := New(parent, agentRegistry(t, child)).Run(context.Background(), Options{
		Prompt:       "delegate",
		Model:        "claude-sonnet-5",
		MaxBudgetUSD: 5,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "max_budget" {
		t.Errorf("stop reason %q, want max_budget (cost %v)", res.StopReason, res.CostUSD)
	}
	if parent.n != 1 {
		t.Errorf("parent made %d requests, want 1 — it ran again after the budget was spent", parent.n)
	}
}

// The child is handed what the parent has left, not an unbounded budget, and
// stops with max_budget when its own requests spend it.
func TestChildReceivesRemainingBudget(t *testing.T) {
	child := &pricedProvider{out: 1_000_000}
	sp := NewSpawner(child, tools.NewRegistry(), anthropic.Model("claude-sonnet-5"), permission.Context{}, nil, 5).
		WithTypes(subagent.Builtin())
	left := 5.0
	_, usage, err := sp.spawn(context.Background(), ChildSpec{Budget: &left}, "Explore", "look", nil)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if child.calls != 1 {
		t.Errorf("child made %d requests, want 1 — the budget did not stop it", child.calls)
	}
	if usage == nil || usage.CostUSD < left {
		t.Errorf("child usage %+v, want a cost at or over the %v budget", usage, left)
	}
}

// A spent budget refuses the launch. Passing 0 through would mean "no budget"
// to the loop, so the child would spend without limit.
func TestSpentBudgetRefusesTheLaunch(t *testing.T) {
	child := &pricedProvider{out: 1_000_000}
	sp := NewSpawner(child, tools.NewRegistry(), anthropic.Model("claude-sonnet-5"), permission.Context{}, nil, 0).
		WithTypes(subagent.Builtin())
	spent := 0.0
	_, _, err := sp.spawn(context.Background(), ChildSpec{Budget: &spent}, "Explore", "look", nil)
	if err == nil {
		t.Fatal("a child was launched with nothing left to spend")
	}
	if child.calls != 0 {
		t.Errorf("the refused child made %d requests", child.calls)
	}
}

// A background child's spend is not on the Agent tool result — it arrives
// later, through CollectChildUsage, when the result is delivered. It still
// has to reach the parent's CostUSD, because that is the figure the budget
// check reads. Two deliveries in one drain must both count: the fold keys
// them by position, and noteChild keeps the first per key.
func TestBackgroundChildCostReachesParentCost(t *testing.T) {
	parent := &scriptedProvider{turns: []anthropic.BetaMessage{
		{StopReason: "end_turn", Content: []anthropic.BetaContentBlockUnion{{Type: "text", Text: "done"}}},
	}}
	one, _ := api.CostUSD("claude-sonnet-5", api.Usage{OutputTokens: 1_000_000})
	delivered := []*tools.ChildUsage{
		{Model: "claude-sonnet-5", OutputTokens: 1_000_000},
		{Model: "claude-sonnet-5", OutputTokens: 1_000_000},
	}
	res, err := New(parent, tools.NewRegistry()).Run(context.Background(), Options{
		Prompt: "go",
		Model:  "claude-sonnet-5",
		// The fold runs when a background result is delivered, at the same
		// point the report reaches the model. Without a report there is
		// nothing to fold.
		CollectBackground: func() string { return "agent-1 finished" },
		CollectChildUsage: func() []*tools.ChildUsage {
			out := delivered
			delivered = nil
			return out
		},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CostUSD != 2*one {
		t.Errorf("CostUSD = %v, want %v — both delivered children counted", res.CostUSD, 2*one)
	}
	if len(res.Children) != 2 {
		t.Errorf("children = %d, want both deliveries", len(res.Children))
	}
}
