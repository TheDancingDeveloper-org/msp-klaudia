package gitguard

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Policy is what an autonomous run does when it starts on a tree that already
// has uncommitted changes to tracked files.
type Policy string

const (
	// Allow is the default: the run coexists with the pre-existing changes and
	// leaves them uncommitted, and the guard bars it from discarding or
	// staging them. Iterating with uncommitted work present must just work.
	Allow Policy = "allow"
	// Commit (opt-in) records the pre-existing changes as a commit of their
	// own on the run's branch first, so they are preserved and separable.
	Commit Policy = "commit"
	// Refuse (opt-in) stops before doing anything when there are any.
	Refuse Policy = "refuse"
)

// Policies lists the accepted values, for help text.
const Policies = "allow|commit|refuse"

// ParsePolicy reads a policy name; "" is Allow.
func ParsePolicy(s string) (Policy, error) {
	switch p := Policy(strings.ToLower(strings.TrimSpace(s))); p {
	case "":
		return Allow, nil
	case Refuse, Commit, Allow:
		return p, nil
	}
	return "", fmt.Errorf("unknown dirty-tree policy %q (want %s)", s, Policies)
}

// RefuseError explains why a run under the opt-in Refuse policy will not start; how says how to choose
// another policy in the caller's frontend (a flag, or a /goal run argument).
// It is nil when there are no pre-existing tracked changes — untracked files
// alone do not stop a run, since the guard keeps `git clean` off them.
func (b *Baseline) RefuseError(how string) error {
	if b == nil || len(b.Tracked) == 0 {
		return nil
	}
	const show = 10
	list := b.Tracked
	more := ""
	if len(list) > show {
		more = fmt.Sprintf(", and %d more", len(list)-show)
		list = list[:show]
	}
	return fmt.Errorf("the working tree already has uncommitted changes to %d tracked file(s): %s%s. "+
		"--loop-dirty=refuse (or /goal run … refuse) was asked to stop in that case. "+
		"Drop it to run alongside them (the default: they are left uncommitted and the loop may not discard them), or %s",
		len(b.Tracked), strings.Join(list, ", "), more, how)
}

// CommitPreexisting commits every tracked change in the repository as one
// commit on the current branch, before the run makes any of its own. It runs
// plain git (hooks included): this is a commit the user asked for. Untracked
// files are left alone — they may be build output or secrets, and committing
// them is not something to do on someone's behalf.
func (b *Baseline) CommitPreexisting() (string, error) {
	if b == nil || len(b.Tracked) == 0 {
		return "", nil
	}
	run := func(args ...string) (string, error) {
		c := exec.Command("git", args...)
		c.Dir = b.Root
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("add", "-u", "--", "."); err != nil {
		return "", fmt.Errorf("git add -u: %v: %s", err, out)
	}
	msg := "wip: uncommitted changes from before the goal loop\n\n" +
		"Committed by klaudia --loop-dirty=commit so the loop's own commits stay separable.\n"
	if out, err := run("commit", "-q", "-m", msg); err != nil {
		return "", fmt.Errorf("git commit: %v: %s", err, out)
	}
	sha, _ := run("rev-parse", "--short", "HEAD")
	return sha, nil
}

// Begin is the first half of a run's entry: it records the uncommitted state
// of the tree at cwd, minus the run's own files (own, absolute paths), and
// under Refuse returns the refusal when that state includes tracked changes.
// how is RefuseError's advice. Outside a repository it returns (nil, nil): no
// git, nothing git can discard. Call it before the run changes anything.
func Begin(cwd string, policy Policy, how string, own ...string) (*Baseline, error) {
	b, err := Capture(cwd)
	if errors.Is(err, ErrNotRepo) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the working tree: %w", err)
	}
	b = b.Without(own...)
	if policy == Refuse {
		if rerr := b.RefuseError(how); rerr != nil {
			return nil, rerr
		}
	}
	return b, nil
}

// Settle is the second half, once the run has (or has not) moved onto its own
// branch: under Commit it commits the pre-existing tracked changes there and
// re-reads the tree, and it returns the baseline the guard should enforce for
// the rest of the run plus a line for the user ("" when there is nothing to
// say). Commit refuses when the run is not on its own branch, rather than
// commit to the branch the user was working on.
func (b *Baseline) Settle(policy Policy, onOwnBranch bool, own ...string) (*Baseline, string, error) {
	if b.Empty() || len(b.Tracked) == 0 {
		return b, "", nil
	}
	if policy != Commit {
		return b, fmt.Sprintf("leaving %d pre-existing change(s) uncommitted; the loop may not discard them", len(b.Tracked)), nil
	}
	if !onOwnBranch {
		return nil, "", errors.New("not committing pre-existing changes: the loop could not move onto its own branch, " +
			"and they would land on the branch you were working on")
	}
	n := len(b.Tracked)
	sha, err := b.CommitPreexisting()
	if err != nil {
		return nil, "", err
	}
	after, err := Capture(b.Root)
	if err != nil {
		return nil, "", fmt.Errorf("reading the working tree: %w", err)
	}
	return after.Without(own...), fmt.Sprintf("committed %d pre-existing change(s) as %s", n, sha), nil
}

// Guard returns b's CheckTool, or nil when there is nothing to protect.
func (b *Baseline) Guard() func(tool string, input []byte, cwd string) string {
	if b.Empty() {
		return nil
	}
	return b.CheckTool
}
