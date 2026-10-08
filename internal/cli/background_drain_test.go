package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// childProvider answers a sub-agent's one model call with "child done", after
// release is closed (nil: at once), or never if its context ends first.
type childProvider struct{ release chan struct{} }

func (p childProvider) StreamTurn(ctx context.Context, _ anthropic.BetaMessageNewParams, _ api.StreamSink) (anthropic.BetaMessage, error) {
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return anthropic.BetaMessage{}, ctx.Err()
		}
	}
	var msg anthropic.BetaMessage
	err := json.Unmarshal([]byte(`{"id":"m1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"child done"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1}}`), &msg)
	return msg, err
}

// launch starts one background Explore agent on provider and returns the
// registry it is tracked in.
func launch(t *testing.T, provider api.Provider) *agent.BackgroundRegistry {
	t.Helper()
	perm := permission.Context{Mode: permission.StaticMode(permission.ModeBypassPermissions)}
	s := agent.NewSpawner(provider, tools.NewRegistry(), "claude-opus-4-8", perm, nil, 0)
	if _, _, err := s.SpawnBackground("", nil, "Explore", "look", "look around", nil); err != nil {
		t.Fatal(err)
	}
	return s.Background()
}

func noFollowUp(t *testing.T) followUpFunc {
	return func(context.Context, []anthropic.BetaMessageParam, int, float64) (agent.Result, error) {
		t.Error("a follow-up turn ran when none should have")
		return agent.Result{}, nil
	}
}

// A follow-up turn gets what is left of the run's caps, and the run's totals
// are summed over both turns.
func TestDrainBackgroundRunsFollowUpWithinCaps(t *testing.T) {
	reg := launch(t, childProvider{})
	first := agent.Result{NumTurns: 2, CostUSD: 0.25, InputTokens: 10, Text: "launched", StopReason: "end_turn"}

	calls := 0
	run := func(_ context.Context, _ []anthropic.BetaMessageParam, maxTurns int, budget float64) (agent.Result, error) {
		calls++
		if maxTurns != 3 || budget != 1.75 {
			t.Errorf("follow-up caps = %d turns, $%g; want 3 turns, $1.75", maxTurns, budget)
		}
		// The loop collects the report itself; do what it would.
		if report := reg.PendingReport(); !strings.Contains(report, "child done") {
			t.Errorf("report = %q", report)
		}
		return agent.Result{NumTurns: 1, CostUSD: 0.5, InputTokens: 5, Text: "final", StopReason: "end_turn"}, nil
	}
	var warned []string
	res, err := drainBackground(context.Background(), reg, first, nil,
		drainLimits{maxTurns: 5, maxBudgetUSD: 2, wait: 5 * time.Second}, run,
		func(m string) { warned = append(warned, m) })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("follow-up ran %d times, want 1", calls)
	}
	if res.Text != "final" || res.NumTurns != 3 || res.CostUSD != 0.75 || res.InputTokens != 15 {
		t.Errorf("summed result = %+v", res)
	}
	if len(warned) != 0 {
		t.Errorf("warned on a delivered run: %v", warned)
	}
}

// A child that outlives the wait is stopped and named, and no turn runs.
func TestDrainBackgroundGivesUpAfterWait(t *testing.T) {
	reg := launch(t, childProvider{release: make(chan struct{})}) // never released
	var warned []string
	_, _ = drainBackground(context.Background(), reg, agent.Result{NumTurns: 1, StopReason: "end_turn"}, nil,
		drainLimits{wait: 50 * time.Millisecond}, noFollowUp(t),
		func(m string) { warned = append(warned, m) })
	if len(warned) != 1 || !strings.Contains(warned[0], "agent-1 (Explore: look around)") ||
		!strings.Contains(warned[0], "still running after 50ms") {
		t.Fatalf("warnings = %q", warned)
	}
	// Abandoned means stopped, not left running behind the exit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a, _ := reg.Get("agent-1"); a.Done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the abandoned agent was not cancelled")
}

// A spent cap stops the drain before any waiting, and says which cap.
func TestDrainBackgroundRespectsCaps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first agent.Result
		err   error
		lim   drainLimits
		want  string
	}{
		{"max turns", agent.Result{NumTurns: 4, StopReason: "end_turn"}, nil, drainLimits{maxTurns: 4}, "--max-turns 4"},
		{"max budget", agent.Result{NumTurns: 1, CostUSD: 1, StopReason: "max_budget"}, nil, drainLimits{maxBudgetUSD: 1}, "--max-budget-usd 1"},
		{"failed", agent.Result{NumTurns: 1}, context.DeadlineExceeded, drainLimits{}, "the run failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			reg := launch(t, childProvider{release: release})
			var warned []string
			_, err := drainBackground(context.Background(), reg, tc.first, tc.err,
				drainLimits{maxTurns: tc.lim.maxTurns, maxBudgetUSD: tc.lim.maxBudgetUSD, wait: time.Minute},
				noFollowUp(t), func(m string) { warned = append(warned, m) })
			if err != tc.err {
				t.Errorf("err = %v, want the first turn's %v", err, tc.err)
			}
			if len(warned) != 1 || !strings.Contains(warned[0], tc.want) || !strings.Contains(warned[0], "agent-1") {
				t.Errorf("warnings = %q, want one naming agent-1 and %q", warned, tc.want)
			}
		})
	}
}

// Nothing launched, nothing to do: the first turn's outcome passes through.
func TestDrainBackgroundNoAgentsIsANoOp(t *testing.T) {
	reg := agent.NewBackgroundRegistry()
	first := agent.Result{Text: "answer", NumTurns: 1}
	res, err := drainBackground(context.Background(), reg, first, nil, drainLimits{wait: time.Minute}, noFollowUp(t),
		func(m string) { t.Errorf("warned: %s", m) })
	if err != nil || res.Text != "answer" || res.NumTurns != 1 {
		t.Errorf("got %+v, %v", res, err)
	}
}

// A wait of zero warns and returns at once, even though a child is still
// running: --background-wait 0 means the caller chose not to wait.
func TestDrainBackgroundZeroWaitExits(t *testing.T) {
	reg := launch(t, childProvider{release: make(chan struct{})})
	var warned []string
	start := time.Now()
	res, err := drainBackground(context.Background(), reg, agent.Result{Text: "answer"}, nil,
		drainLimits{wait: 0}, noFollowUp(t), func(m string) { warned = append(warned, m) })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Text != "answer" {
		t.Errorf("result text = %q, want the first turn's", res.Text)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "without waiting") {
		t.Errorf("warnings = %q, want one saying it did not wait", warned)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a zero wait blocked")
	}
}
