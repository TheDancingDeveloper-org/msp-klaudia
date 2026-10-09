package gitguard

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/greenthread-ai/klaudia/internal/gitprobe"
)

// A baseline describes one repository: the one the run started in. A git
// command aimed somewhere else — `git -C ../other add -A`, `cd ../wt && git
// add .`, a linked worktree of the same repository — cannot touch a single
// path in it, and judging that command by the start repository's changes
// refused work that had nothing to do with them (#289): agents each committing
// in a worktree of their own were all blocked by untracked files in the
// checkout the session started in.
//
// So every git invocation is resolved to the work tree it actually runs in,
// and judged by that work tree's baseline. Another repository's baseline is
// captured the first time the run touches it — a Bash call whose working
// directory or `cd` lands in it, a git command aimed at it, a file written in
// it — and always before the touching call is allowed, so a run whose first
// act in a repository is `git reset --hard` there finds the user's changes
// already recorded. Capturing at first touch rather than first git command is
// what lets a run commit its own files: a file the run writes before it ever
// runs git in that tree is written after the capture, so it is the run's, not
// the user's.
//
// Work trees are compared by their canonical top level, not by git's common
// directory: a linked worktree shares the common directory with its main
// checkout but has a working tree, an index and untracked files of its own.
//
// Where the target cannot be read — a `cd "$X"`, a `cd` that may not have run,
// GIT_DIR or GIT_WORK_TREE in the environment, a wrapper that changes
// directory — the command is judged against the start repository with every
// protected path in reach, which is how every command was judged before.

// site is where a git invocation runs: the baseline whose paths it can reach
// (nil when none), and its directory, for resolving relative pathspecs.
type site struct {
	base *Baseline
	dir  string
}

// locate resolves dir — with any --git-dir/--work-tree options the command
// passes — to the work tree git would use, capturing that work tree's baseline
// if this is the run's first touch of it.
func (b *Baseline) locate(dir string, gitOpts []string) site {
	top, prefix, err := probe(dir, gitOpts)
	if err != nil {
		// Not in a work tree git can find, so a git command there fails
		// before it changes anything. A directory under the start
		// repository's root that git cannot read stays the start
		// repository's, which is the conservative reading.
		if len(gitOpts) == 0 && b.contains(dir) {
			return site{b, dir}
		}
		return site{}
	}
	if canonical(top) == canonical(b.Root) {
		return site{b, filepath.Join(b.Root, filepath.FromSlash(prefix))}
	}
	o := b.other(top)
	if o == nil {
		return site{}
	}
	return site{o, filepath.Join(o.Root, filepath.FromSlash(prefix))}
}

// touch records the baseline of the work tree containing path, if it is not
// the start repository and has not been recorded yet. path need not exist; its
// nearest existing ancestor is used.
func (b *Baseline) touch(path string) {
	dir := existingDir(path)
	if dir == "" || dir == b.Root {
		return
	}
	b.locate(dir, nil)
}

// other returns the baseline of the repository rooted at top, capturing it on
// first use. The lock is held across the capture so that two calls touching a
// new repository at once both wait for the one baseline taken before either
// ran.
func (b *Baseline) other(top string) *Baseline {
	key := canonical(top)
	b.mu.Lock()
	defer b.mu.Unlock()
	if o, ok := b.others[key]; ok {
		return o
	}
	o, err := Capture(top)
	if err != nil {
		return nil // not remembered: the next touch tries again
	}
	if b.others == nil {
		b.others = map[string]*Baseline{}
	}
	b.others[key] = o
	return o
}

// contains reports whether p is lexically inside b.Root.
func (b *Baseline) contains(p string) bool {
	r, err := filepath.Rel(b.Root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, "../")
}

// probe asks git for the top level of the work tree dir belongs to and dir's
// path inside it ("" at the top level).
func probe(dir string, gitOpts []string) (top, prefix string, err error) {
	args := append(append([]string{}, gitOpts...), "rev-parse", "--show-toplevel", "--show-prefix")
	out, err := gitprobe.Command(dir, args...).Output()
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	top = lines[0]
	if len(lines) > 1 {
		prefix = lines[1]
	}
	return top, prefix, nil
}

// canonical resolves symlinks, so two spellings of one directory compare
// equal.
func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// existingDir returns p if it is a directory, else its nearest existing
// ancestor directory, or "" when there is none.
func existingDir(p string) string {
	for d := filepath.Clean(p); ; {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
