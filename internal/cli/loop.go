package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/greenthread-ai/klaudia/internal/api"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/gitguard"
	"github.com/greenthread-ai/klaudia/internal/goal"
	"github.com/greenthread-ai/klaudia/internal/hooks"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// loopStallLimit stops the loop after this many consecutive iterations make no
// progress — no new git commit, or under --no-commit no change to the spec — a
// sign the agent is spinning.
const loopStallLimit = 3

// loopRun bundles the already-built run state the goal loop reuses (set up once
// in run() like the single-shot headless path).
type loopRun struct {
	loop         *agent.Loop
	cwd          string
	mode         permission.Mode
	model        anthropic.Model
	provider     string // configured provider name, for the model-aware defaults
	effort       string
	thinking     string
	system       string
	maxTurns     int
	maxBudgetUSD float64
	iterations   int
	dirty        gitguard.Policy // what to do about pre-existing uncommitted changes
	runMode      goal.RunMode    // --no-branch / --no-commit; the spec can add to it
	permCtx      permission.Context
	hostGate     *agent.HostGate
	approver     agent.Approver
	deferred     map[string]bool
	recorder     agent.Recorder
	onSummary    func(string)
	hooks        *hooks.Runner
	render       *Renderer
	diagnostics  tools.DiagnosticsFunc
}

// goalRetryBackoff is how long the goal loop waits before each retry of an
// iteration that failed transiently. A var so tests need not wait.
var goalRetryBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}

// retryTransient runs turn, and runs it again after each goalRetryBackoff
// pause while it fails transiently (api.IsTransient). A goal run is often
// hours long, and one overloaded response used to end all of it; an iteration
// re-reads the spec and git, so repeating one is safe. Other errors, and a
// cancelled ctx, return at once.
func retryTransient(ctx context.Context, errOut io.Writer, what string, turn func() (agent.Result, error)) (agent.Result, error) {
	res, err := turn()
	for retry := 0; err != nil && api.IsTransient(err) && retry < len(goalRetryBackoff); retry++ {
		wait := goalRetryBackoff[retry]
		fmt.Fprintf(errOut, "  %s failed (%v); retrying in %s (%d/%d)\n", what, err, wait, retry+1, len(goalRetryBackoff))
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(wait):
		}
		res, err = turn()
	}
	return res, err
}

// runGoalLoop drives the headless Ralph loop: it iterates against the goal spec,
// each iteration re-orienting from the spec + git with a fresh context (progress
// lives in files and commits, not the conversation window), until the model
// reports completion, the iteration cap is hit, or it stalls (no new commits).
func runGoalLoop(ctx context.Context, cmd *cobra.Command, p loopRun) error {
	specText, specPath, _ := goal.Read(p.cwd)
	if strings.TrimSpace(specText) == "" {
		msg := "no goal spec found — create .klaudia/GOAL.md first (run klaudia interactively and use /goal)"
		if prd, ok := goal.IgnoredPRD(p.cwd); ok {
			msg += "; " + goal.IgnoredPRDNote(prd)
		}
		return errors.New(msg)
	}
	// No human is present to approve tool use, so the loop can only do real work
	// in a mode that does not ask. Autonomous qualifies now that the host gate
	// is what stops a host change: before it existed, the only way to run
	// unattended was --dangerously-skip-permissions, which turned off every
	// check to get past prompts about editing files in the project.
	if p.mode != permission.ModeAutonomous && p.mode != permission.ModeBypassPermissions {
		return fmt.Errorf("--loop runs unattended and cannot answer prompts; use --permission-mode autonomous "+
			"(host changes still need --allow-host-changes) or --dangerously-skip-permissions. Current mode: %s", p.mode)
	}

	iters := goal.Iterations(p.iterations)
	errOut := cmd.ErrOrStderr()

	// The git model: a branch and a commit per iteration for code goals; the
	// flags, or a `mode: artifact` line in the spec, drop either (#246).
	runMode := goal.SpecMode(specText).Merge(p.runMode)
	if runMode.NoBranch && p.dirty == gitguard.Commit {
		return errors.New("--loop-dirty=commit commits pre-existing changes to the goal branch, and this run has none " +
			"(--no-branch, or the spec's mode: artifact); use the default --loop-dirty=allow, or commit them yourself")
	}
	if s := runMode.String(); s != "" {
		fmt.Fprintf(errOut, "↳ %s\n", s)
	}

	// Pre-existing uncommitted work is recorded before anything moves, so the
	// guard can keep the loop off it for the whole run: the first production
	// run reverted 14 such files to HEAD while undoing a one-line change of its
	// own (#250). The loop runs alongside it by default; it never requires a
	// clean tree. The spec is the loop's own file.
	baseline, err := gitguard.Begin(p.cwd, p.dirty, "use --loop-dirty=commit to commit them to the goal branch first.", specPath)
	if err != nil {
		return fmt.Errorf("--loop: %w", err)
	}

	branch := goal.BranchName(specText)
	base := gitBranch(p.cwd) // merge target (the branch we start from)
	if base == branch {
		base = "" // resuming on the goal branch already; base unknown
	}
	onBranch := false
	if runMode.NoBranch {
		// Stay where we are: no branch to merge, so no merge hint either.
	} else if out, gerr := gitCheckoutBranch(p.cwd, branch); gerr != nil {
		fmt.Fprintf(errOut, "warning: not branching (%s)\n", strings.TrimSpace(out))
	} else {
		onBranch = true
		fmt.Fprintf(errOut, "↳ on branch %s\n", branch)
	}
	baseline, note, err := baseline.Settle(p.dirty, onBranch, specPath)
	if err != nil {
		return fmt.Errorf("--loop-dirty=%s: %w", p.dirty, err)
	}
	if note != "" {
		fmt.Fprintf(errOut, "↳ %s\n", note)
	}

	mergeHint := func() {
		if onBranch {
			fmt.Fprintln(errOut, goal.MergeHint(branch, base))
		}
	}

	emit := withNotices(func(ev agent.Event) { _ = p.render.Event(ev); warnOn(errOut, FormatText, ev) }, errOut)
	// Each turn starts fresh (no InitialMessages): it re-reads the spec and git
	// state, per the Ralph principle (bounded context over long runs).
	runTurn := func(prompt string) (agent.Result, error) {
		return p.loop.Run(ctx, agent.Options{
			WorkingDir:    p.cwd,
			Prompt:        prompt,
			Model:         p.model,
			Effort:        p.effort,
			Thinking:      p.thinking,
			ProviderName:  p.provider,
			System:        p.system,
			MaxTurns:      p.maxTurns,
			MaxBudgetUSD:  p.maxBudgetUSD,
			Permission:    p.permCtx,
			Host:          p.hostGate,
			Approver:      p.approver,
			DeferredTools: p.deferred,
			Recorder:      p.recorder,
			WebTools:      true,
			OnSummary:     p.onSummary,
			Diagnostics:   p.diagnostics,
			Hooks:         p.hooks,
			CommandGuard:  baseline.Guard(),
		}, emit)
	}

	// Stall detection watches HEAD for a code goal. A no-commit run makes no
	// commits by design, so it watches the spec instead: every iteration is
	// told to record its step in the spec's Progress section. "" (no repo,
	// unreadable spec) never stalls.
	progress, stallWhat := func() string { return gitCommit(p.cwd) }, "no new commits"
	if runMode.NoCommit {
		progress, stallWhat = func() string { return fileDigest(specPath) }, "the spec has not changed"
	}
	lastProgress := progress()
	stalls := 0
	for i := 1; i <= iters; i++ {
		fmt.Fprintf(errOut, "↻ iteration %d/%d\n", i, iters)
		res, err := retryTransient(ctx, errOut, fmt.Sprintf("iteration %d", i), func() (agent.Result, error) {
			return runTurn(goal.IterationPromptFor(specPath, runMode))
		})
		if err != nil {
			return err
		}
		if goal.IsComplete(res.Text) {
			fmt.Fprintf(errOut, "✓ goal complete in %d iteration(s)\n", i)
			mergeHint()
			return nil
		}
		if c := progress(); c == "" || c != lastProgress {
			stalls, lastProgress = 0, c
		} else if stalls++; stalls >= loopStallLimit {
			fmt.Fprintf(errOut, "⊘ %s for %d iterations — stopping (goal not complete).\n", stallWhat, stalls)
			break
		}
	}

	// Stopped without completing (cap or stall): one wrap-up turn records an
	// end-of-run summary in the spec so the next run (or a person) can resume.
	fmt.Fprintf(errOut, "summarising progress to %s…\n", specPath)
	if _, err := runTurn(goal.WrapUpPromptFor(specPath, runMode)); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "stopped; goal not yet complete. Progress recorded in %s — re-run to resume.\n", specPath)
	mergeHint()
	return nil
}

// fileDigest returns a hash of path's contents, or "" when it cannot be read.
func fileDigest(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// gitRun runs a git command in dir and returns its combined output.
func gitRun(dir string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

// gitCheckoutBranch switches to branch, reusing it if it already exists (so a
// resumed loop continues from earlier commits) or creating it otherwise.
func gitCheckoutBranch(dir, branch string) (string, error) {
	if _, err := gitRun(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return gitRun(dir, "checkout", branch)
	}
	return gitRun(dir, "checkout", "-b", branch)
}
