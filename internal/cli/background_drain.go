package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/session"
)

// A headless (-p) run has one user turn, and the process exits when it ends.
// A background sub-agent launched in that turn reports "on a later turn" — and
// there was none: the result was dropped and the run exited 0 with the model
// still promising to report it (#276). drainBackground supplies the later
// turn: it waits for the run's outstanding background agents and hands their
// results to the model as a follow-up turn, repeating while the model launches
// more, until nothing is outstanding or a limit is reached.

// headlessBackgroundWait bounds how long a -p run waits, in total, for
// background sub-agents after its turn ends. A sub-agent is a whole child loop,
// so this is generous; it exists so a hung child cannot hold a pipeline open
// forever. A variable so tests can shorten it.
var headlessBackgroundWait = 10 * time.Minute

// backgroundPollInterval is how often the wait checks the registry. Sub-agents
// take seconds at the least, so a tenth of a second costs nothing noticeable.
var backgroundPollInterval = 100 * time.Millisecond

// drainLimits are the run-wide caps the follow-up turns must stay inside.
// maxTurns and maxBudgetUSD are for the whole run, first turn included, as the
// user gave them; 0 means unlimited, as for agent.Options.
type drainLimits struct {
	maxTurns     int
	maxBudgetUSD float64
	wait         time.Duration
	// cwd and sessionID name where the children are persisted, so a later
	// --resume can say which were still running. Empty skips persistence.
	cwd       string
	sessionID string
}

// persistChildren writes the registry's children beside the session.
func persistChildren(reg *agent.BackgroundRegistry, lim drainLimits) {
	if lim.sessionID == "" {
		return
	}
	var recs []session.ChildRecord
	for _, a := range reg.List() {
		recs = append(recs, session.ChildRecord{
			ID: a.ID, Type: a.Type, Label: a.Label, Status: string(a.Status),
			Background: a.Background, Provenance: a.Provenance, Result: a.Result, Err: a.Err,
		})
	}
	_ = session.WriteChildren(lim.cwd, lim.sessionID, recs)
}

// followUpFunc runs one follow-up turn on history, inside the remaining caps
// (0 = unlimited). The turn's new user message is the background report, which
// the loop collects itself through Options.CollectBackground.
type followUpFunc func(ctx context.Context, history []anthropic.BetaMessageParam, maxTurns int, maxBudgetUSD float64) (agent.Result, error)

// drainBackground delivers the background sub-agents still owed to a finished
// headless run. res and err are the first turn's outcome; the returned pair is
// the whole run's, with token counts, cost, turns and API time summed across
// every turn and the text, stop reason and messages taken from the last.
//
// It gives up — emitting a warning that names each agent whose result is lost,
// and stopping those still running — when a turn failed or was cut short, when
// the turn or budget cap is used up, when ctx ends, or when the wait expires.
func drainBackground(ctx context.Context, reg *agent.BackgroundRegistry, res agent.Result, err error, lim drainLimits, run followUpFunc, warn func(string)) (agent.Result, error) {
	defer persistChildren(reg, lim)
	if lim.wait == 0 {
		running, ready := reg.Undelivered("")
		if n := len(running) + len(ready); n > 0 && warn != nil {
			warn(fmt.Sprintf("%d background sub-agent(s) still running; exiting without waiting (--background-wait 0)", n))
		}
		return res, err
	}
	deadline := time.Now().Add(lim.wait)
	for {
		running, ready := reg.Undelivered("")
		if len(running)+len(ready) == 0 {
			return res, err
		}
		if reason := drainBlocked(ctx, res, err, lim); reason != "" {
			abandonBackground(reg, append(ready, running...), reason, warn)
			return res, err
		}
		ready, running = waitBackground(ctx, reg, deadline)
		if len(running) > 0 {
			reason := fmt.Sprintf("they were still running after %s", lim.wait)
			if ctx.Err() != nil {
				reason = "the run was interrupted"
			}
			abandonBackground(reg, running, reason, warn)
		}
		if len(ready) == 0 {
			return res, err
		}
		next, nerr := run(ctx, res.Messages, remainingTurns(lim.maxTurns, res.NumTurns), remainingBudget(lim.maxBudgetUSD, res.CostUSD))
		res, err = sumResults(res, next), nerr
	}
}

// drainBlocked says why no follow-up turn may run, or "" when one may.
func drainBlocked(ctx context.Context, res agent.Result, err error, lim drainLimits) string {
	switch {
	case ctx.Err() != nil:
		return "the run was interrupted"
	case err != nil:
		return "the run failed"
	case res.StopReason == "max_turns", lim.maxTurns > 0 && res.NumTurns >= lim.maxTurns:
		return fmt.Sprintf("the run reached --max-turns %d", lim.maxTurns)
	case res.StopReason == "max_budget", lim.maxBudgetUSD > 0 && res.CostUSD >= lim.maxBudgetUSD:
		return fmt.Sprintf("the run reached --max-budget-usd %g", lim.maxBudgetUSD)
	case res.StopReason == "blocked_by_hook":
		return "a hook stopped the run"
	}
	return ""
}

// waitBackground waits until every outstanding agent has finished, ctx ends,
// or the deadline passes, and reports which are ready to deliver and which are
// still running. It waits for all of them rather than delivering each as it
// lands: nobody is watching a -p run's intermediate turns, and one report
// costs one model turn where several would cost several.
func waitBackground(ctx context.Context, reg *agent.BackgroundRegistry, deadline time.Time) (ready, running []agent.BackgroundAgent) {
	tick := time.NewTicker(backgroundPollInterval)
	defer tick.Stop()
	for {
		running, ready = reg.Undelivered("")
		if len(running) == 0 || !time.Now().Before(deadline) {
			return ready, running
		}
		select {
		case <-ctx.Done():
			return reg.Undelivered("")
		case <-tick.C:
		}
	}
}

// abandonBackground stops the agents whose results will not be delivered and
// names them in a warning, so the loss is visible instead of silent.
func abandonBackground(reg *agent.BackgroundRegistry, lost []agent.BackgroundAgent, reason string, warn func(string)) {
	if len(lost) == 0 {
		return
	}
	names := make([]string, 0, len(lost))
	for _, a := range lost {
		reg.Cancel(a.ID)
		name := fmt.Sprintf("%s (%s", a.ID, a.Type)
		if a.Label != "" {
			name += ": " + a.Label
		}
		names = append(names, name+")")
	}
	warn(fmt.Sprintf("background sub-agent result(s) not delivered because %s: %s",
		reason, strings.Join(names, ", ")))
}

func remainingTurns(limit, used int) int {
	if limit <= 0 {
		return 0
	}
	return limit - used
}

func remainingBudget(limit, spent float64) float64 {
	if limit <= 0 {
		return 0
	}
	return limit - spent
}

// sumResults folds a follow-up turn into the run so far. Counters add up; what
// describes the end of the run comes from the latest turn.
func sumResults(sofar, next agent.Result) agent.Result {
	next.NumTurns += sofar.NumTurns
	next.InputTokens += sofar.InputTokens
	next.OutputTokens += sofar.OutputTokens
	next.CacheReadInputTokens += sofar.CacheReadInputTokens
	next.CacheCreationInputTokens += sofar.CacheCreationInputTokens
	next.APIDuration += sofar.APIDuration
	next.CostUSD += sofar.CostUSD
	if next.Messages == nil {
		next.Messages = sofar.Messages
	}
	return next
}
