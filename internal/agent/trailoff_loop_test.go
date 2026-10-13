package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// scripted is a Provider that returns canned assistant messages in order and
// records the text of every user message it was shown, so a test can see the
// nudge land.
type scripted struct {
	replies []string
	seen    []string
}

func (s *scripted) StreamTurn(_ context.Context, params anthropic.BetaMessageNewParams, sink api.StreamSink) (anthropic.BetaMessage, error) {
	for _, m := range params.Messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.OfText != nil {
				s.seen = append(s.seen, b.OfText.Text)
			}
		}
	}
	text := "(script exhausted)"
	if len(s.replies) > 0 {
		text, s.replies = s.replies[0], s.replies[1:]
	}
	sink.OnText(text)
	raw := fmt.Sprintf(`{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":%q}]}`, text)
	var msg anthropic.BetaMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return msg, err
	}
	return msg, nil
}

func runScript(t *testing.T, replies []string, cont bool) (Result, *scripted) {
	t.Helper()
	return runScriptOpts(t, replies, Options{ContinueOnTrailOff: cont})
}

func runScriptOpts(t *testing.T, replies []string, opts Options) (Result, *scripted) {
	t.Helper()
	s := &scripted{replies: replies}
	opts.Prompt = "do the thing"
	res, err := New(s, tools.NewRegistry()).Run(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, s
}

func TestTrailOffNudgeThenDone(t *testing.T) {
	res, s := runScript(t, []string{"I'll read loop.go next.", "Read it. The exit is at the empty tool list."}, true)
	if res.TrailOffNudges != 1 {
		t.Fatalf("TrailOffNudges = %d, want 1", res.TrailOffNudges)
	}
	if res.NumTurns != 2 {
		t.Fatalf("NumTurns = %d, want 2", res.NumTurns)
	}
	if !strings.Contains(res.Text, "Read it") {
		t.Fatalf("final text = %q, want the second reply", res.Text)
	}
	if !strings.Contains(strings.Join(s.seen, "\n"), "made no tool call") {
		t.Fatalf("the nudge never reached the model; seen %q", s.seen)
	}
}

func TestTrailOffBounded(t *testing.T) {
	res, _ := runScript(t, []string{
		"I'll read loop.go next.",
		"Next I'll run the tests.",
		"Let me check the output now.",
	}, true)
	if res.TrailOffNudges != maxTrailOffNudges {
		t.Fatalf("TrailOffNudges = %d, want the cap %d", res.TrailOffNudges, maxTrailOffNudges)
	}
	// The cap stops the loop: one turn per nudge plus the turn that stands.
	if res.NumTurns != maxTrailOffNudges+1 {
		t.Fatalf("NumTurns = %d, want %d", res.NumTurns, maxTrailOffNudges+1)
	}
	if res.Text != "Let me check the output now." {
		t.Fatalf("the turn past the cap should stand, got %q", res.Text)
	}
}

func TestTrailOffOffByDefault(t *testing.T) {
	res, s := runScript(t, []string{"I'll read loop.go next."}, false)
	if res.TrailOffNudges != 0 || res.NumTurns != 1 {
		t.Fatalf("with the guard off: nudges %d turns %d, want 0 and 1", res.TrailOffNudges, res.NumTurns)
	}
	if strings.Contains(strings.Join(s.seen, "\n"), "made no tool call") {
		t.Fatal("nudged a turn the guard was not asked to watch")
	}
}

func TestTrailOffIgnoresARealEnding(t *testing.T) {
	for _, reply := range []string{
		"All done.\nAUTOPILOT: DONE",
		"Which file should I read?",
		"The tests pass.",
	} {
		res, _ := runScript(t, []string{reply}, true)
		if res.TrailOffNudges != 0 || res.NumTurns != 1 {
			t.Errorf("%q: nudges %d turns %d, want a plain ending", reply, res.TrailOffNudges, res.NumTurns)
		}
	}
}

// A nudge is another request, so it is bound by --max-turns like a tool turn.
// Before the fix MaxTurns=1 with three announcements gave three turns.
func TestTrailOffRespectsMaxTurns(t *testing.T) {
	replies := []string{"I'll read loop.go next.", "Next I'll run the tests.", "Let me check the output now."}
	res, _ := runScriptOpts(t, replies, Options{ContinueOnTrailOff: true, MaxTurns: 1})
	if res.NumTurns != 1 || res.TrailOffNudges != 0 {
		t.Fatalf("MaxTurns=1: turns %d nudges %d, want 1 and 0", res.NumTurns, res.TrailOffNudges)
	}
	res, _ = runScriptOpts(t, replies, Options{ContinueOnTrailOff: true, MaxTurns: 2})
	if res.NumTurns != 2 || res.TrailOffNudges != 1 {
		t.Fatalf("MaxTurns=2: turns %d nudges %d, want 2 and 1", res.NumTurns, res.TrailOffNudges)
	}
}

func TestWithinLimits(t *testing.T) {
	cases := []struct {
		opts Options
		res  Result
		want bool
	}{
		{Options{}, Result{NumTurns: 50, CostUSD: 9}, true},
		{Options{MaxTurns: 3}, Result{NumTurns: 2}, true},
		{Options{MaxTurns: 3}, Result{NumTurns: 3}, false},
		{Options{MaxBudgetUSD: 1}, Result{CostUSD: 0.5}, true},
		{Options{MaxBudgetUSD: 1}, Result{CostUSD: 1}, false},
	}
	for _, c := range cases {
		if got := withinLimits(c.opts, c.res); got != c.want {
			t.Errorf("withinLimits(%+v, turns %d cost %v) = %v, want %v", c.opts, c.res.NumTurns, c.res.CostUSD, got, c.want)
		}
	}
}

// The review's conditional offer must end the run: nudging it under -p would
// tell the model to push without the approval it asked for.
func TestTrailOffLeavesAConditionalOffer(t *testing.T) {
	res, _ := runScript(t, []string{"Done. If you approve, I'll push the branch."}, true)
	if res.TrailOffNudges != 0 || res.NumTurns != 1 {
		t.Fatalf("nudges %d turns %d, want a plain ending", res.TrailOffNudges, res.NumTurns)
	}
}
