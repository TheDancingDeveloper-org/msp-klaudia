package gitguard

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	top, prefix, gitDir, err := b.probe(dir, gitOpts)
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
	// A work tree whose state could not be read is not one to let a
	// command loose in unprotected. Fail closed: judge it as an
	// unreadable target is judged, against the start repository with
	// every protected path in reach.
	o := b.other(top, gitDir)
	if o == nil {
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
//
// A baseline is remembered only while the tree is still the tree it was
// captured for. The identity — the .git entry and the configs of the git dir it
// resolved to — is re-read from the filesystem before the baseline is handed
// back, and it is keyed by the tree's top, so it holds whichever directory the
// command was aimed at (#295). A tree that no longer matches is not
// re-captured: the remembered baseline is dropped and the command is refused,
// because re-capturing would record files the run wrote since the first touch
// as the user's. The next touch, finding nothing remembered, captures afresh.
func (b *Baseline) other(top, gitDir string) *Baseline {
	key := canonical(top)
	b.mu.Lock()
	defer b.mu.Unlock()
	if o, ok := b.others[key]; ok {
		if o.id.same(key) {
			return o
		}
		delete(b.others, key)
		return nil
	}
	o, err := Capture(top)
	if err != nil {
		return nil // not remembered: the next touch tries again
	}
	if id, ok := identityOf(key, gitDir); ok {
		o.id = id
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
// directory that was asked about, and the absolute git dir it resolved to.
type probed struct{ top, prefix, gitDir string }

// gitIdentity is what a remembered baseline is trusted on: the .git entry at
// the tree's top, and the config files of the git dir it resolved to. It is
// keyed by the tree's top, not by the directory a command was aimed at, so a
// first touch at the top and a command run from a subdirectory are judged by
// the same identity (#295).
//
// The .git entry is recorded by what cannot be forged from userland and does
// not move during ordinary git work: its type, and its device and inode. A
// directory's mtime changes whenever git renames a file inside it — index.lock
// to index on a status refresh, an add, a commit — and stamping that would drop
// the baseline and re-record the run's own files as the user's. A `gitdir:`
// file is recorded by its content, which is exactly the pointer a swap
// rewrites. The config files are recorded by inode and ctime, which `touch`
// cannot set back.
type gitIdentity struct {
	dotGit   entryStamp
	gitdir   string // content of a .git file; "" when .git is a directory
	configs  []configStamp
	noConfig bool // no config file could be read: the identity is not trustworthy
}

// entryStamp records a .git entry's type and, when it could be read, the
// device and inode it occupies.
type entryStamp struct {
	exists bool
	isDir  bool
	dev    uint64
	ino    uint64
	known  bool // dev and ino were readable
}

// configStamp records one config file by the device and inode it occupies and
// its ctime, neither of which userland can forge.
type configStamp struct {
	path string
	dev  uint64
	ino  uint64
	nsec int64
}

// identityOf reads the identity of the work tree rooted at top that resolves
// through gitDir. ok is false when no identity can be taken — there is no .git
// entry, or no git dir to read — in which case the baseline is still captured
// but not trusted past this call.
func identityOf(top, gitDir string) (gitIdentity, bool) {
	id := gitIdentity{dotGit: stampEntry(filepath.Join(top, ".git"))}
	if !id.dotGit.exists {
		return id, false
	}
	if !id.dotGit.isDir {
		content, err := os.ReadFile(filepath.Join(top, ".git"))
		if err != nil {
			return id, false
		}
		id.gitdir = string(content)
	}
	if gitDir == "" {
		return id, false
	}
	id.configs = stampConfigs(gitDir)
	id.noConfig = len(id.configs) == 0
	return id, !id.noConfig
}

// stampEntry reads the type, device and inode of p without following a symlink.
func stampEntry(p string) entryStamp {
	fi, err := os.Lstat(p)
	if err != nil {
		return entryStamp{}
	}
	s := entryStamp{exists: true, isDir: fi.IsDir()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.dev, s.ino, s.known = uint64(st.Dev), st.Ino, true
	}
	return s
}

// stampConfigs reads the config files of a git dir: its own config, the common
// config a linked worktree shares with its main checkout, and config.worktree
// where one is present. A path that does not exist is not stamped; a path that
// exists but cannot be read fails the whole set, so the identity is not trusted.
func stampConfigs(gitDir string) []configStamp {
	paths := []string{filepath.Join(gitDir, "config")}
	if common := commonDir(gitDir); common != "" && canonical(common) != canonical(gitDir) {
		paths = append(paths, filepath.Join(common, "config"))
	}
	paths = append(paths, filepath.Join(gitDir, "config.worktree"))
	var out []configStamp
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		out = append(out, configStamp{path: p, dev: uint64(st.Dev), ino: st.Ino, nsec: int64(st.Ctim.Sec)*1e9 + int64(st.Ctim.Nsec)})
	}
	return out
}

// commonDir reads the commondir a linked worktree's git dir names, or "" when
// there is none.
func commonDir(gitDir string) string {
	content, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	rel := strings.TrimSpace(string(content))
	if rel == "" {
		return ""
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(gitDir, rel)
}

// same reports whether id still describes the work tree rooted at top. It
// re-reads the filesystem and does not run git: a .git entry replaced by a
// gitdir file, a gitdir file rewritten, or a config rewritten in place all
// fail it, while a status, add or commit — which only rename files inside the
// .git directory — does not.
func (id gitIdentity) same(top string) bool {
	if id.noConfig {
		return false
	}
	cur, ok := identityOf(top, id.configDir())
	if !ok {
		return false
	}
	return id.dotGit == cur.dotGit && id.gitdir == cur.gitdir && stampsEqual(id.configs, cur.configs)
}

// configDir is the git dir the config stamps were taken from: the directory of
// the first, which stampConfigs always records as the git dir's own config.
func (id gitIdentity) configDir() string {
	if len(id.configs) == 0 {
		return ""
	}
	return filepath.Dir(id.configs[0].path)
}

// stampsEqual reports whether the two config sets name the same files with the
// same inode and ctime. Order follows stampConfigs, so it is compared directly.
func stampsEqual(a, b []configStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// probe is runProbe, remembered by canonical directory so that the touch every
// Bash call makes does not run git each time. Only answers found are kept, and
// only for a plain directory (no --git-dir/--work-tree): "not a work tree" can
// stop being true — a `git init` or `git worktree add` there — and a stale
// "no" would let a command into that tree unprotected, where a stale "yes"
// only keeps the tree that was there protected.
//
// Whether the tree is still the tree the remembered baseline describes is not
// decided here. That identity is kept on the baseline, keyed by the tree's top,
// and re-checked in other whatever directory the command was aimed at.
func (b *Baseline) probe(dir string, gitOpts []string) (top, prefix, gitDir string, err error) {
	if len(gitOpts) > 0 {
		return runProbe(dir, gitOpts)
	}
	key := canonical(dir)
	if p, ok := b.probes.Load(key); ok {
		old := p.(probed)
		return old.top, old.prefix, old.gitDir, nil
	}
	top, prefix, gitDir, err = runProbe(dir, nil)
	if err == nil && gitDir != "" {
		b.probes.Store(key, probed{top, prefix, gitDir})
	}
	return top, prefix, gitDir, err
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
