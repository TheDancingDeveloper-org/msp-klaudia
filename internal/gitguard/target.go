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
	all  bool // the paths reached cannot be told: judge every protected path
}

// locate resolves dir — with any --git-dir/--work-tree options the command
// passes — to the work tree git would use, capturing that work tree's baseline
// if this is the run's first touch of it.
func (b *Baseline) locate(dir string, gitOpts []string) site {
	top, prefix, err := b.probe(dir, gitOpts)
	if err != nil {
		// Not in a work tree git can find, so a git command there fails
		// before it changes anything. A directory under the start
		// repository's root that git cannot read stays the start
		// repository's, which is the conservative reading.
		if len(gitOpts) == 0 && b.contains(dir) {
			return site{base: b, dir: dir}
		}
		return site{}
	}
	if canonical(top) == canonical(b.Root) {
		return site{base: b, dir: filepath.Join(b.Root, filepath.FromSlash(prefix))}
	}
	o := b.other(top)
	if o == nil {
		// A work tree whose state could not be read is not one to let a
		// command loose in unprotected. Fail closed: judge it as an
		// unreadable target is judged, against the start repository with
		// every protected path in reach.
		return site{base: b, dir: dir, all: true}
	}
	return site{base: o, dir: filepath.Join(o.Root, filepath.FromSlash(prefix))}
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

// probed is a work tree found by probe: its top level, the prefix of the
// directory that was asked about, and the identity the answer was taken under.
// The identity is the .git entry's type and mtime plus the mtime of the config
// inside the git dir it resolved to. A directory's path does not change when
// its .git is swapped for a `gitdir:` file pointing at the start repository, or
// when that git dir's config gains a core.worktree, but this does (#295).
type probed struct {
	top, prefix string
	id          gitIdentity
}

// gitIdentity is what probe re-checks before trusting a remembered answer: the
// .git entry as it stands, and the config of the git dir it pointed at.
type gitIdentity struct {
	dotGit     fileStamp
	config     fileStamp
	configPath string
}

// fileStamp records whether a path exists, whether it is a directory, and when
// it was last modified.
type fileStamp struct {
	exists bool
	isDir  bool
	mtime  int64
}

func stampOf(p string) fileStamp {
	fi, err := os.Lstat(p)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, isDir: fi.IsDir(), mtime: fi.ModTime().UnixNano()}
}

// identityOf reads the identity of the work tree at dir that resolves through
// gitDir. The config stamp follows a `gitdir:` file to the git dir it names, so
// a config written in the repository that file points at is seen here.
func identityOf(dir, gitDir string) gitIdentity {
	return gitIdentity{dotGit: stampOf(filepath.Join(dir, ".git")), config: stampOf(filepath.Join(gitDir, "config")), configPath: gitDir}
}

// same reports whether id still describes the work tree at dir.
func (id gitIdentity) same(dir string) bool {
	return id == identityOf(dir, id.configPath)
}

// probe is runProbe, remembered by canonical directory so that the touch every
// Bash call makes does not run git each time. Only answers found are kept, and
// only for a plain directory (no --git-dir/--work-tree): "not a work tree" can
// stop being true — a `git init` or `git worktree add` there — and a stale
// "no" would let a command into that tree unprotected, where a stale "yes"
// only keeps the tree that was there protected.
//
// A remembered answer is reused only while the tree is still the tree it was
// taken for. Before it is returned the tree's identity — its .git entry and its
// git dir's config — is re-read from the filesystem, which needs no git. A tree
// whose .git was replaced, or whose config was rewritten to point its work tree
// elsewhere, no longer matches, so the stale baseline is dropped and the tree
// is probed again.
func (b *Baseline) probe(dir string, gitOpts []string) (top, prefix string, err error) {
	if len(gitOpts) > 0 {
		top, prefix, _, err = runProbe(dir, gitOpts)
		return top, prefix, err
	}
	key := canonical(dir)
	if p, ok := b.probes.Load(key); ok {
		old := p.(probed)
		if old.id.same(dir) {
			return old.top, old.prefix, nil
		}
		b.probes.Delete(key)
		b.forget(old.top)
	}
	top, prefix, gitDir, err := runProbe(dir, nil)
	if err == nil {
		b.probes.Store(key, probed{top, prefix, identityOf(dir, gitDir)})
	}
	return top, prefix, err
}

// forget drops the baseline captured for the work tree rooted at top, so the
// next touch re-captures it. A tree whose git dir has changed is no longer the
// tree that baseline describes.
func (b *Baseline) forget(top string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.others, canonical(top))
}

// runProbe asks git for the top level of the work tree dir belongs to, dir's
// path inside it ("" at the top level), and the absolute git dir it resolved
// to. A variable so a test can count the calls.
var runProbe = func(dir string, gitOpts []string) (top, prefix, gitDir string, err error) {
	args := append(append([]string{}, gitOpts...), "rev-parse", "--show-toplevel", "--show-prefix", "--absolute-git-dir")
	out, err := gitprobe.Command(dir, args...).Output()
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	top = lines[0]
	if len(lines) > 1 {
		prefix = lines[1]
	}
	if len(lines) > 2 {
		gitDir = lines[2]
	}
	return top, prefix, gitDir, nil
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
