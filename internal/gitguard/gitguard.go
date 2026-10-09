// Package gitguard keeps an autonomous run from discarding work it does not
// own. It records which paths already had uncommitted changes when the run
// began (the baseline) and refuses shell commands whose git invocations would
// throw those changes away: `git checkout -- <path>`, `git restore`,
// `git reset --hard`, `git clean -f`, `git stash`, `git rm -f`, and the forced
// branch switches.
//
// The goal loop is the caller. Its first production run undid a one-line edit
// of its own with `git checkout -- <14 files>`, and that reverted every one of
// those files to HEAD — including uncommitted work that predated the run and
// was never in git (#250). The loop's prompt asks the model not to do that;
// this is what holds when the model does it anyway.
//
// A baseline protects the repository it was captured in. Commands aimed at
// another repository or worktree are judged by that one's own baseline,
// captured the first time the run touches it (see target.go).
//
// The reading is deliberately conservative. When a command's paths cannot be
// read — an expansion, a `cd` that may not have run, paths arriving through
// xargs, an unparsable line — it is assumed to reach every protected path. A
// false refusal costs the model one retry with a narrower command; a false
// allowance costs someone's afternoon.
package gitguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/greenthread-ai/klaudia/internal/gitprobe"
	"github.com/greenthread-ai/klaudia/internal/native/bashparser"
	"github.com/greenthread-ai/klaudia/internal/trust"
)

// Baseline is the set of paths that had uncommitted changes at a moment in
// time, relative to the repository root, with '/' separators. A directory entry
// (an untracked directory git reports as a whole) ends in '/'.
type Baseline struct {
	Root      string   // absolute repository root
	Tracked   []string // tracked paths modified, staged, deleted or renamed
	Untracked []string // untracked (not ignored) files and directories

	mu     sync.Mutex
	others map[string]*Baseline // other work trees the run has touched, by canonical root
	probes sync.Map             // canonical dir → probed, see Baseline.probe
}

// ErrNotRepo is returned by Capture outside a git work tree.
var ErrNotRepo = errors.New("not a git repository")

// Capture records the uncommitted state of the repository containing dir.
func Capture(dir string) (*Baseline, error) {
	out, err := gitprobe.Command(dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, ErrNotRepo
	}
	root := strings.TrimSpace(string(out))
	st, err := gitprobe.Command(root, "status", "--porcelain=v1", "-z", "--untracked-files=normal").Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	b := &Baseline{Root: root}
	fields := bytes.Split(st, []byte{0})
	for i := 0; i < len(fields); i++ {
		f := string(fields[i])
		if len(f) < 4 {
			continue
		}
		code, path := f[:2], f[3:]
		switch {
		case code == "??":
			b.Untracked = append(b.Untracked, path)
		case code == "!!":
		default:
			b.Tracked = append(b.Tracked, path)
			// A rename or copy is followed by its source path; the source is
			// part of the change too.
			if code[0] == 'R' || code[0] == 'C' {
				if i+1 < len(fields) {
					i++
					b.Tracked = append(b.Tracked, string(fields[i]))
				}
			}
		}
	}
	sort.Strings(b.Tracked)
	sort.Strings(b.Untracked)
	return b, nil
}

// Without returns a copy of b with the given absolute paths dropped — for the
// files the caller itself owns, like the goal spec the loop is about to commit.
func (b *Baseline) Without(abs ...string) *Baseline {
	drop := map[string]bool{}
	for _, p := range abs {
		if rel, ok := b.rel(p); ok {
			drop[rel] = true
		}
	}
	keep := func(in []string) []string {
		var out []string
		for _, p := range in {
			if !drop[p] {
				out = append(out, p)
			}
		}
		return out
	}
	return &Baseline{Root: b.Root, Tracked: keep(b.Tracked), Untracked: keep(b.Untracked)}
}

// Empty reports whether nothing is protected.
func (b *Baseline) Empty() bool { return b == nil || len(b.Tracked)+len(b.Untracked) == 0 }

func (b *Baseline) rel(p string) (string, bool) {
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(filepath.Clean(p)), true
	}
	r, err := filepath.Rel(b.Root, p)
	if err != nil || r == ".." || strings.HasPrefix(r, "../") {
		return "", false
	}
	return filepath.ToSlash(r), true
}

// CheckTool is the agent's CommandGuard: it returns a refusal for a Bash call
// that would discard protected changes, and "" for anything else. A file
// write is never refused, but it is a first touch of the work tree it lands
// in, so that tree's baseline is captured before the write happens.
func (b *Baseline) CheckTool(tool string, input []byte, cwd string) string {
	if b == nil {
		return ""
	}
	switch tool {
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &in) != nil || strings.TrimSpace(in.Command) == "" {
			return ""
		}
		return b.CheckCommand(in.Command, cwd)
	case "Write", "Edit", "NotebookEdit":
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		if json.Unmarshal(input, &in) != nil {
			return ""
		}
		p := in.FilePath
		if p == "" {
			p = in.NotebookPath
		}
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		b.touch(p)
	}
	return ""
}

// CheckCommand returns why cmd may not run, or "" when it may.
func (b *Baseline) CheckCommand(cmd, cwd string) string {
	if b == nil {
		return ""
	}
	b.touch(cwd)
	hits, what, stage := b.scan(cmd, cwd, true, 0)
	if len(hits) == 0 {
		return ""
	}
	if stage {
		return stageRefusal(what, hits)
	}
	return refusal(what, hits)
}

// maxDepth bounds how far nested `sh -c` payloads are followed.
const maxDepth = 4

// scan returns the protected paths cmd could discard and the git invocation
// that would do it. cwdKnown is false when the directory cmd runs in could not
// be read (a nested shell after an unreadable cd).
func (b *Baseline) scan(cmd, cwd string, cwdKnown bool, depth int) ([]string, string, bool) {
	a, err := bashparser.Parse(cmd)
	if err != nil || depth > maxDepth {
		// Unreadable: refuse only if it plausibly runs a discarding git command.
		if mentionsDiscard(cmd) {
			return b.all(), "an unparsable command that mentions a discarding git subcommand", false
		}
		return nil, "", false
	}
	// These move git somewhere this reader does not follow, whether set on the
	// command, through env, or exported earlier: GIT_DIR and GIT_WORK_TREE the
	// repository, GIT_INDEX_FILE the index a command writes, GIT_COMMON_DIR the
	// refs, and core.worktree (set by `git config` earlier on the line) the work
	// tree a later command touches (#293, #295). Any of them keeps the reading
	// on the start repository.
	envMoved := false
	for _, moved := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "core.worktree"} {
		envMoved = envMoved || strings.Contains(cmd, moved)
	}
	// dir is where the next command runs, while known. pending is the chain
	// whose cd set it without being sure to run: once past that chain, dir
	// is no longer known.
	dir, known, pending := cwd, cwdKnown, -1
	for _, c := range a.Commands {
		if pending >= 0 && c.Chain != pending {
			known, pending = false, -1
		}
		if payload, ok := bashparser.ShellPayload(c.Name, c.Args); ok {
			if hits, what, stage := b.scan(payload, dir, known, depth+1); len(hits) > 0 {
				return hits, what, stage
			}
			continue
		}
		name, args, ok := trust.Unwrapped(c)
		if !ok {
			continue
		}
		words := c.ArgWords[len(c.ArgWords)-len(args):]
		switch bashparser.Base(name) {
		case "cd", "pushd", "popd":
			dir, known, pending = b.chdir(a, c, words, dir, known)
			continue
		case "git":
		default:
			continue
		}
		viaXargs := bashparser.Base(c.Name) == "xargs"
		literal := true
		for _, w := range c.ArgWords {
			literal = literal && w.Literal
		}
		args, gdir, gitOpts, readable := globalOpts(args, words, dir)
		if len(args) == 0 {
			continue
		}
		d := discard(args[0], args[1:])
		if d == nil {
			continue
		}
		// An unreadable target is judged against the start repository, with
		// every protected path in reach. So is any --git-dir or --work-tree:
		// the index a command writes belongs to the git dir, which need not be
		// the work tree locate would find, so following the work tree could
		// judge a command clean while it rewrites the start repository's
		// index (#293).
		at, pathsKnown := site{base: b, dir: gdir}, false
		if known && readable && !envMoved && !chdirWrapped(c) && len(gitOpts) == 0 {
			at = b.locate(gdir, nil)
			pathsKnown = !at.all
		}
		if at.base.Empty() {
			continue
		}
		var hits []string
		if d.all || viaXargs || !literal || !pathsKnown {
			hits = at.base.pick(d, nil, true)
		} else {
			hits = at.base.pick(d, at.base.resolve(d.paths, at.dir), len(d.paths) == 0)
		}
		if len(hits) > 0 {
			what := "git " + strings.Join(append([]string{args[0]}, d.flags...), " ")
			if at.base != b {
				what += " in " + at.base.Root + ", a work tree this run first touched with uncommitted changes already in it,"
			}
			return hits, what, d.stage
		}
	}
	return nil, "", false
}

// chdir follows a cd. The new directory is known only when the line is
// sequential, the cd names one literal, existing directory, and either starts
// its chain — so it runs whatever came before — or the commands that read the
// directory are later in its own `&&` chain, which run only if it succeeded.
// Anything else (pushd, popd, `cd -`, `cd ~`, an expansion, a cd in a subshell,
// pipeline or conditional) leaves the directory unknown. A known target is
// touched: if it is another work tree, its baseline is captured now.
func (b *Baseline) chdir(a bashparser.Analysis, c bashparser.Command, words []bashparser.Word, dir string, known bool) (string, bool, int) {
	if !a.Sequential || bashparser.Base(c.Name) != "cd" || len(words) != 1 || !words[0].Literal {
		return dir, false, -1
	}
	t := words[0].Text
	if t == "" || strings.HasPrefix(t, "-") || strings.HasPrefix(t, "~") {
		return dir, false, -1
	}
	if !filepath.IsAbs(t) {
		if !known {
			return dir, false, -1
		}
		t = filepath.Join(dir, t)
	}
	if !isDir(t) {
		return dir, false, -1
	}
	b.touch(t)
	if c.ChainStart {
		return t, true, -1
	}
	return t, true, c.Chain
}

// chdirWrapped reports whether a wrapper in front of git changes directory
// first (`env -C dir git …`, `sudo -D dir git …`).
func chdirWrapped(c bashparser.Command) bool {
	if bashparser.Base(c.Name) == "git" {
		return false
	}
	for _, a := range c.Args {
		if bashparser.Base(a) == "git" {
			return false
		}
		if strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-D") || strings.HasPrefix(a, "--chdir") {
			return true
		}
	}
	return false
}

// discardSpec describes what a git subcommand invocation can throw away.
type discardSpec struct {
	paths     []string // pathspecs, as written
	all       bool     // reaches the whole tree whatever the pathspecs say
	tracked   bool     // can discard changes to tracked files
	stage     bool     // stages or commits them rather than discarding them
	untracked bool     // can remove untracked files
	flags     []string // for the message
}

// discard classifies one git subcommand. It returns nil when the invocation
// cannot discard uncommitted work.
func discard(sub string, args []string) *discardSpec {
	flags, paths, dashdash := splitArgs(args)
	has := func(names ...string) bool {
		for _, f := range flags {
			for _, n := range names {
				if f == n || strings.HasPrefix(f, n+"=") {
					return true
				}
			}
		}
		return false
	}
	shortHas := func(r byte) bool {
		for _, f := range flags {
			if len(f) > 1 && f[0] == '-' && f[1] != '-' && strings.IndexByte(f[1:], r) >= 0 {
				return true
			}
		}
		return false
	}
	switch sub {
	case "checkout":
		if has("-b", "-B", "--orphan") && !dashdash && !has("-f", "--force") {
			return nil // creating a branch keeps the working tree
		}
		if has("-f", "--force") {
			return &discardSpec{all: true, tracked: true, flags: []string{"--force"}}
		}
		if dashdash {
			return &discardSpec{paths: pathsAfterDashDash(args), tracked: true, flags: []string{"--"}}
		}
		// `git checkout <x>` is a branch switch (which git refuses over local
		// changes) or a path restore. Treat every argument as a possible path:
		// a branch only collides if a protected file shares its name.
		if len(paths) == 0 {
			return nil
		}
		return &discardSpec{paths: paths, tracked: true}
	case "restore":
		staged, worktree := has("--staged", "-S") || shortHas('S'), has("--worktree", "-W") || shortHas('W')
		if staged && !worktree {
			return nil // only unstages; the working tree is left alone
		}
		return &discardSpec{paths: paths, tracked: true}
	case "reset":
		if has("--hard", "--merge", "--keep") {
			return &discardSpec{all: true, tracked: true, flags: []string{"--hard"}}
		}
		return nil
	case "clean":
		if has("-n", "--dry-run") || shortHas('n') {
			return nil
		}
		if has("-f", "--force") || shortHas('f') {
			return &discardSpec{paths: paths, untracked: true, flags: []string{"-f"}}
		}
		return nil
	case "stash":
		mode := "push"
		rest := args
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			mode, rest = args[0], args[1:]
		}
		switch mode {
		case "push", "save":
			f2, p2, _ := splitArgs(rest)
			d := &discardSpec{paths: p2, tracked: true, flags: []string{mode}}
			if mode == "save" {
				d.paths = nil // save takes a message, not pathspecs
			}
			for _, f := range f2 {
				if f == "-u" || f == "--include-untracked" || f == "-a" || f == "--all" {
					d.untracked = true
				}
			}
			return d
		}
		return nil
	case "rm":
		if has("-f", "--force") || shortHas('f') {
			if has("--cached") {
				return nil
			}
			return &discardSpec{paths: paths, tracked: true, flags: []string{"-f"}}
		}
		return nil
	case "switch":
		if has("-f", "--force", "--discard-changes") {
			return &discardSpec{all: true, tracked: true, flags: []string{"--discard-changes"}}
		}
		return nil
	// Staging is not destruction, but sweeping pre-existing changes into the
	// run's commit takes them out of the user's hands and mixes them with the
	// run's work; the run commits only what it changed.
	case "add":
		if has("-n", "--dry-run") || shortHas('n') {
			return nil
		}
		if has("-A", "--all") || shortHas('A') {
			return &discardSpec{all: true, tracked: true, untracked: true, stage: true, flags: []string{"-A"}}
		}
		if has("-u", "--update") || shortHas('u') {
			return &discardSpec{paths: paths, tracked: true, stage: true, flags: []string{"-u"}}
		}
		if len(paths) == 0 {
			return nil
		}
		return &discardSpec{paths: paths, tracked: true, untracked: true, stage: true}
	case "commit":
		if has("-a", "--all") || shortHas('a') {
			return &discardSpec{all: true, tracked: true, stage: true, flags: []string{"-a"}}
		}
		if len(paths) == 0 {
			return nil // commits what is staged; the add that staged it was checked
		}
		return &discardSpec{paths: paths, tracked: true, stage: true}
	}
	return nil
}

// splitArgs separates flags from positional arguments. Everything after `--`
// is positional. Flags that take a separate value consume it.
func splitArgs(args []string) (flags, positional []string, dashdash bool) {
	valued := map[string]bool{"-s": true, "--source": true, "-m": true, "--message": true,
		"-b": true, "-B": true, "--orphan": true, "-e": true, "--exclude": true,
		"--pathspec-from-file": true, "--conflict": true, "-F": true, "--file": true, "-C": true,
		"-c": true, "--author": true, "--date": true, "--fixup": true, "--squash": true, "--chmod": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return flags, append(positional, args[i+1:]...), true
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			if valued[a] && i+1 < len(args) {
				i++
			}
		default:
			positional = append(positional, a)
		}
	}
	return flags, positional, false
}

func pathsAfterDashDash(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// globalOpts strips git's global options, following -C to the directory the
// subcommand runs in and collecting --git-dir and --work-tree (in their
// original spelling) for locate. words are args with their Literal flags;
// readable is false when an option that moves git is not a literal.
func globalOpts(args []string, words []bashparser.Word, dir string) (rest []string, at string, gitOpts []string, readable bool) {
	readable = true
	take := func(n int) {
		for _, w := range words[:n] {
			readable = readable && w.Literal
		}
		args, words = args[n:], words[n:]
	}
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "-C" && len(args) > 1:
			d := args[1]
			if !filepath.IsAbs(d) {
				d = filepath.Join(dir, d)
			}
			dir = d
			take(2)
		case (a == "--git-dir" || a == "--work-tree") && len(args) > 1:
			gitOpts = append(gitOpts, a, args[1])
			take(2)
		case strings.HasPrefix(a, "--work-tree=") || strings.HasPrefix(a, "--git-dir="):
			gitOpts = append(gitOpts, a)
			take(1)
		case (a == "-c" || a == "--namespace") && len(args) > 1:
			args, words = args[2:], words[2:]
		case strings.HasPrefix(a, "-"):
			args, words = args[1:], words[1:]
		default:
			return args, dir, gitOpts, readable
		}
	}
	return nil, dir, gitOpts, readable
}

// resolve turns pathspecs (relative to dir) into repository-relative paths. A
// pathspec git would read as magic or a glob resolves to "" — everything.
func (b *Baseline) resolve(specs []string, dir string) []string {
	var out []string
	for _, s := range specs {
		if strings.HasPrefix(s, ":") || strings.ContainsAny(s, "*?[") {
			out = append(out, "")
			continue
		}
		p := s
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		r, ok := b.rel(p)
		if !ok {
			continue // outside this repository
		}
		if r == "." {
			r = ""
		}
		out = append(out, r)
	}
	return out
}

// pick returns the protected paths d can discard: those specs reach, or every
// one when all is set. A "" spec reaches everything.
func (b *Baseline) pick(d *discardSpec, specs []string, all bool) []string {
	var pool []string
	if d.tracked {
		pool = append(pool, b.Tracked...)
	}
	if d.untracked {
		pool = append(pool, b.Untracked...)
	}
	var hits []string
	for _, p := range pool {
		if all || overlaps(specs, p) {
			hits = append(hits, p)
		}
	}
	return hits
}

func (b *Baseline) all() []string { return append(append([]string{}, b.Tracked...), b.Untracked...) }

// overlaps reports whether any pathspec names p, a directory containing p, or
// (for an untracked directory entry) something inside it.
func overlaps(specs []string, p string) bool {
	pd := strings.TrimSuffix(p, "/")
	for _, s := range specs {
		s = strings.TrimSuffix(s, "/")
		if s == "" || s == pd || strings.HasPrefix(pd, s+"/") || strings.HasPrefix(s, pd+"/") {
			return true
		}
	}
	return false
}

// mentionsDiscard is the fallback for a line the parser cannot read.
func mentionsDiscard(cmd string) bool {
	if !strings.Contains(cmd, "git") {
		return false
	}
	for _, w := range []string{"checkout", "restore", "reset", "clean", "stash", " rm ", "switch", " add ", "commit"} {
		if strings.Contains(cmd, w) {
			return true
		}
	}
	return false
}

func stageRefusal(what string, hits []string) string {
	list, more := shortList(hits)
	return fmt.Sprintf("Refused: %s would stage or commit uncommitted changes that existed before this run began "+
		"and are the user's, not yours: %s%s. They stay uncommitted. Commit only your own work: "+
		"`git add <the files you changed>` (not -A, ., -u or commit -a), leaving out the files in that list.",
		what, strings.Join(list, ", "), more)
}

func shortList(hits []string) ([]string, string) {
	const show = 8
	if len(hits) > show {
		return hits[:show], fmt.Sprintf(" (and %d more)", len(hits)-show)
	}
	return hits, ""
}

func refusal(what string, hits []string) string {
	const show = 8
	list := hits
	more := ""
	if len(list) > show {
		more = fmt.Sprintf(" (and %d more)", len(list)-show)
		list = list[:show]
	}
	return fmt.Sprintf("Refused: %s would discard uncommitted changes that existed before this run began "+
		"and are not yours to throw away: %s%s. "+
		"To undo your own change, edit it back, or `git checkout -- <path>` only the files you changed "+
		"that are not in that list, or `git revert` your own commit. Never discard, reset, clean or "+
		"stash over pre-existing work.", what, strings.Join(list, ", "), more)
}
