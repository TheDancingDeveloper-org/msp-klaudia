package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/goal"
)

// gitInit makes the project a repository with one commit on main.
func gitInit(t *testing.T, e *cliEnv) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"commit", "-q", "-m", "spec"},
	} {
		if out, err := gitRun(e.Dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A goal the model reports complete stops the loop after that iteration, on
// its own branch, with a hint naming the branch to merge back into.
func TestLoopStopsWhenGoalComplete(t *testing.T) {
	m := newFakeModel(t, say("all done\n"+goal.CompleteToken))
	e := newCLIEnv(t, m)
	e.write("PRD.md", "# Build the widget\n\n- [ ] widget\n\n## Verify\n\nmake test\n")
	gitInit(t, e)

	r := e.run(nil, "--loop", "--dangerously-skip-permissions")
	if r.Err != nil {
		t.Fatal(r.dump())
	}
	if got := gitBranch(e.Dir); got != "klaudia/goal-build-the-widget" {
		t.Errorf("branch = %q, want klaudia/goal-build-the-widget", got)
	}
	for _, want := range []string{"iteration 1/10", "goal complete in 1 iteration", "git merge klaudia/goal-build-the-widget"} {
		if !strings.Contains(r.Stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.Stderr)
		}
	}
	if n := len(m.Requests()); n != 1 {
		t.Errorf("model called %d times, want 1", n)
	}

	// Run again from the goal branch: it is reused rather than recreated, and
	// with no base to name, the hint just says where the work is.
	m2 := newFakeModel(t, say(goal.CompleteToken))
	t.Setenv("KLAUDIA_CUSTOM_ENDPOINT", m2.URL())
	r = e.run(nil, "--loop", "--dangerously-skip-permissions")
	if r.Err != nil {
		t.Fatal(r.dump())
	}
	if !strings.Contains(r.Stderr, "on branch klaudia/goal-build-the-widget") || !strings.Contains(r.Stderr, "review it (git log / git diff)") {
		t.Errorf("resumed loop stderr:\n%s", r.Stderr)
	}
}

// Iterations that make no commits stop the loop after loopStallLimit, and a
// wrap-up turn records progress in the spec.
func TestLoopStopsAfterStalledIterations(t *testing.T) {
	m := newFakeModel(t)
	e := newCLIEnv(t, m)
	e.write("PRD.md", "# Stalling goal\n\n- [ ] keep going\n\n## Verify\n\nmake test\n")
	gitInit(t, e)

	r := e.run(nil, "--loop", "--dangerously-skip-permissions", "--max-iterations", "8")
	if r.Err != nil {
		t.Fatal(r.dump())
	}
	for _, want := range []string{"no new commits for 3 iterations", "summarising progress", "goal not yet complete"} {
		if !strings.Contains(r.Stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.Stderr)
		}
	}
	// Three stalled iterations and one wrap-up.
	reqs := m.Requests()
	if len(reqs) != loopStallLimit+1 {
		t.Fatalf("model called %d times, want %d", len(reqs), loopStallLimit+1)
	}
	if !strings.Contains(reqs[len(reqs)-1].Raw(), "Progress") {
		t.Error("the last turn was not the wrap-up prompt")
	}
}

// Outside a git repository the loop cannot branch or detect stalls; it warns,
// runs to the iteration cap, and wraps up.
func TestLoopOutsideGitRunsToIterationCap(t *testing.T) {
	m := newFakeModel(t)
	e := newCLIEnv(t, m)
	e.write(".klaudia/GOAL.md", "# No repo goal\n")
	r := e.run(nil, "--loop", "--permission-mode", "autonomous", "--max-iterations", "2")
	if r.Err != nil {
		t.Fatal(r.dump())
	}
	if !strings.Contains(r.Stderr, "warning: not branching") {
		t.Errorf("no branching warning:\n%s", r.Stderr)
	}
	if strings.Contains(r.Stderr, "Work is on branch") {
		t.Error("a merge hint was printed for a branch that was never created")
	}
	if n := len(m.Requests()); n != 3 {
		t.Errorf("model called %d times, want 2 iterations + wrap-up", n)
	}
}

// The loop refuses to start when it could not do anything useful.
func TestLoopRefusesWithoutSpecOrUnattendedMode(t *testing.T) {
	t.Run("no spec", func(t *testing.T) {
		m := newFakeModel(t)
		e := newCLIEnv(t, m)
		r := e.run(nil, "--loop", "--dangerously-skip-permissions")
		if r.Err == nil || !strings.Contains(r.Err.Error(), "no goal spec found") {
			t.Fatalf("err = %v", r.Err)
		}
		if len(m.Requests()) != 0 {
			t.Error("model called without a spec")
		}
	})
	t.Run("mode that asks", func(t *testing.T) {
		m := newFakeModel(t)
		e := newCLIEnv(t, m)
		e.write("PRD.md", "# g\n\n- [ ] task\n\n## Verify\n\nmake test\n")
		r := e.run(nil, "--loop", "--permission-mode", "plan")
		if r.Err == nil || !strings.Contains(r.Err.Error(), "cannot answer prompts") {
			t.Fatalf("err = %v", r.Err)
		}
		if len(m.Requests()) != 0 {
			t.Error("model called in a mode the loop cannot run")
		}
	})
}

// An API failure mid-loop ends the loop with that error rather than spinning.
func TestLoopStopsOnAPIError(t *testing.T) {
	m := newFakeModel(t, fakeTurn{Status: 400})
	e := newCLIEnv(t, m)
	e.write("PRD.md", "# g\n\n- [ ] task\n\n## Verify\n\nmake test\n")
	r := e.run(nil, "--loop", "--dangerously-skip-permissions", "--max-iterations", "3")
	if r.Err == nil || r.Code != ExitError {
		t.Fatalf("want an error exit\n%s", r.dump())
	}
	if n := len(m.Requests()); n != 1 {
		t.Errorf("model called %d times after a failure, want 1", n)
	}
}

// dirtyRepo is a goal-loop project whose tree already has uncommitted work
// when the loop starts: a modified tracked file and an untracked one.
func dirtyRepo(t *testing.T, e *cliEnv) {
	t.Helper()
	e.write("PRD.md", "# Dirty goal\n\n- [ ] task\n\n## Verify\n\nmake test\n")
	e.write("user.txt", "committed\n")
	gitInit(t, e)
	e.write("user.txt", userEdit)
	e.write("notes/scratch.txt", "untracked notes\n")
}

func readFile(t *testing.T, e *cliEnv, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.Dir, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(b)
}

const userEdit = "the user's uncommitted afternoon\n"

// The #250 regression, under the operator's scope: the loop starts ON a dirty
// tree — no flag, no clean-tree precondition — and whatever the model runs,
// the unrelated uncommitted change is still there afterwards, byte for byte,
// and still uncommitted. The model here does what the production run did (a
// broad `git checkout --` to undo its own edit), then every other way of
// losing or sweeping up the user's work, in the mode with no permission checks.
func TestLoopKeepsPreexistingChanges(t *testing.T) {
	m := newFakeModel(t,
		use("Bash", map[string]any{"command": "echo mine > own.txt && git add own.txt && git commit -qm own && echo oops >> own.txt"}),
		use("Bash", map[string]any{"command": "git checkout -- own.txt user.txt PRD.md && git status --short"}),
		use("Bash", map[string]any{"command": "git reset --hard"}),
		use("Bash", map[string]any{"command": "git clean -fd"}),
		use("Bash", map[string]any{"command": "git stash -u"}),
		use("Bash", map[string]any{"command": "git add -A && git commit -qm everything"}),
		use("Bash", map[string]any{"command": "git commit -qam everything"}),
		use("Bash", map[string]any{"command": "git checkout -- own.txt"}), // its own file only: allowed
		say(goal.CompleteToken),
	)
	e := newCLIEnv(t, m)
	dirtyRepo(t, e)

	r := e.run(nil, "--loop", "--dangerously-skip-permissions")
	if r.Err != nil {
		t.Fatal(r.dump())
	}
	if got := readFile(t, e, "user.txt"); got != userEdit {
		t.Errorf("user.txt = %q, want the pre-existing change %q unmodified", got, userEdit)
	}
	if out, _ := gitRun(e.Dir, "show", "HEAD:user.txt"); out != "committed\n" {
		t.Errorf("HEAD:user.txt = %q: the pre-existing change was swept into a commit", out)
	}
	if out, _ := gitRun(e.Dir, "status", "--porcelain", "--", "user.txt"); !strings.HasPrefix(out, " M") {
		t.Errorf("user.txt status = %q, want it still an unstaged modification", out)
	}
	if got := readFile(t, e, "notes/scratch.txt"); got != "untracked notes\n" {
		t.Errorf("notes/scratch.txt = %q: the untracked file was removed or changed", got)
	}
	if got := readFile(t, e, "own.txt"); got != "mine\n" {
		t.Errorf("own.txt = %q: the loop's revert of its own file did not run", got)
	}
	if !strings.Contains(r.Stderr, "leaving 1 pre-existing change(s) uncommitted") {
		t.Errorf("stderr lacks the coexist notice:\n%s", r.Stderr)
	}
	reqs := m.Requests()
	if refused := strings.Count(reqs[len(reqs)-1].Raw(), "existed before this run began"); refused != 6 {
		t.Errorf("%d commands refused, want 6", refused)
	}
}

// Refusing a dirty tree is an explicit opt-in, never the default.
func TestLoopRefuseIsOptIn(t *testing.T) {
	m := newFakeModel(t)
	e := newCLIEnv(t, m)
	dirtyRepo(t, e)

	r := e.run(nil, "--loop", "--dangerously-skip-permissions", "--loop-dirty=refuse")
	if r.Err == nil || !strings.Contains(r.Err.Error(), "user.txt") {
		t.Fatalf("err = %v, want a refusal naming user.txt", r.Err)
	}
	if len(m.Requests()) != 0 {
		t.Error("model called after an opt-in refusal")
	}
	if got := gitBranch(e.Dir); got != "main" {
		t.Errorf("branch = %q; a refused loop must not move off main", got)
	}
	if got := readFile(t, e, "user.txt"); got != userEdit {
		t.Errorf("user.txt = %q", got)
	}
}

// --loop-dirty=commit preserves the pre-existing changes as their own commit on
// the goal branch, so even a reset of the loop's work cannot lose them.
func TestLoopCommitsPreexistingChanges(t *testing.T) {
	m := newFakeModel(t,
		use("Bash", map[string]any{"command": "git reset --hard"}),
		say(goal.CompleteToken),
	)
	e := newCLIEnv(t, m)
	dirtyRepo(t, e)

	r := e.run(nil, "--loop", "--dangerously-skip-permissions", "--loop-dirty=commit")
	if r.Err != nil {
		t.Fatal(r.dump())
	}
	if got := gitBranch(e.Dir); got != "klaudia/goal-dirty-goal" {
		t.Errorf("branch = %q", got)
	}
	if out, _ := gitRun(e.Dir, "show", "HEAD:user.txt"); !strings.Contains(out, "afternoon") {
		t.Errorf("HEAD:user.txt = %q; the pre-existing change was not committed", out)
	}
	if out, _ := gitRun(e.Dir, "log", "-1", "--format=%s"); !strings.Contains(out, "before the goal loop") {
		t.Errorf("HEAD subject = %q", out)
	}
	if out, _ := gitRun(e.Dir, "show", "main:user.txt"); strings.Contains(out, "afternoon") {
		t.Error("the pre-existing change was committed to main, not the goal branch")
	}
	if got := readFile(t, e, "notes/scratch.txt"); !strings.Contains(got, "untracked notes") {
		t.Errorf("untracked notes = %q", got)
	}
	if !strings.Contains(r.Stderr, "committed 1 pre-existing change(s)") {
		t.Errorf("stderr:\n%s", r.Stderr)
	}
}

func TestLoopDirtyFlagValidated(t *testing.T) {
	e := newCLIEnv(t, newFakeModel(t))
	r := e.run(nil, "--loop", "--dangerously-skip-permissions", "--loop-dirty", "yolo")
	if r.Err == nil || !strings.Contains(r.Err.Error(), "allow|commit|refuse") {
		t.Fatalf("err = %v", r.Err)
	}
}
