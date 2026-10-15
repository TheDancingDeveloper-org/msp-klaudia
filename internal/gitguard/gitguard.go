// Package gitguard keeps an autonomous run from discarding work it does not
// own. It records which paths already had uncommitted changes when the run
// began (the baseline), with a content hash of each, and refuses shell
// commands whose git invocations would throw those changes away: `git checkout
// -- <path>`, `git restore`, `git reset --hard`, `git clean -f`, `git stash`,
// `git rm -f`, and the forced branch switches.
//
// Staging is judged by those hashes, not by the dirty bit. A baseline path the
// session has edited since capture, and a path git has unmerged because the
// session is resolving a merge, may be staged and committed. A baseline path
// left untouched stays out of the commit. Discarding a baseline path is
// refused either way. A path already staged at capture stays out of a plain
// `git commit` too, which records the whole index, until the session restages
// or unstages it.
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
//
// Work and Index are content hashes of those paths at capture, keyed by the
// same relative path. A later stage or commit of a baseline path is allowed
// only when the bytes the session would record differ from that hash — the
// session edited the path after start — or the path is an unmerged merge
// entry the session is resolving. A path whose hash cannot be re-read stays
// protected. Discarding a baseline path is refused either way.
type Baseline struct {
	Root      string            // absolute repository root
	Tracked   []string          // tracked paths modified, staged, deleted or renamed
	Untracked []string          // untracked (not ignored) files and directories
	Staged    []string          // the Tracked paths with a change in the index at capture
	Work      map[string]string // path → hash of the work-tree bytes at capture
	Index     map[string]string // tracked path → hash of the index blob at capture

	mu     sync.Mutex
	others map[string]*Baseline // other work trees the run has touched, by canonical root
	probes sync.Map             // canonical dir → probed, see Baseline.probe
	id     gitIdentity          // the tree this baseline was captured for; see other
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
			// The X column is the index side: anything but a space is a
			// change a plain `git commit` would record. An unmerged entry is
			// a merge the session may be resolving, not staged work.
			unmerged := code == "DD" || code == "AA" || strings.ContainsRune(code, 'U')
			staged := code[0] != ' ' && !unmerged
			if staged {
				b.Staged = append(b.Staged, path)
			}
			// A rename or copy is followed by its source path; the source is
			// part of the change too.
			if code[0] == 'R' || code[0] == 'C' {
				if i+1 < len(fields) {
					i++
					b.Tracked = append(b.Tracked, string(fields[i]))
					if staged {
						b.Staged = append(b.Staged, string(fields[i]))
					}
				}
			}
		}
	}
	sort.Strings(b.Tracked)
	sort.Strings(b.Untracked)
	sort.Strings(b.Staged)
	b.snapshot()
	return b, nil
}

// snapshot records the work-tree and index hashes of every protected path.
// A path whose bytes cannot be read is stored as hashUnreadable, which never
// compares equal to a later read, so the path stays protected.
func (b *Baseline) snapshot() {
	b.Work = map[string]string{}
	b.Index = map[string]string{}
	for _, p := range b.Untracked {
		b.Work[p] = hashWork(b.Root, p)
	}
	for _, p := range b.Tracked {
		b.Work[p] = hashWork(b.Root, p)
		b.Index[p] = hashIndex(b.Root, p)
	}
}

// hashUnreadable marks a path whose content could not be read. It never
// matches a real hash, so a comparison fails closed.
// hashAbsent marks an index entry git answered for and does not have: the
// session unstaged or removed it, which is a definite answer, not a failure.
const (
	hashUnreadable = "?"
	hashAbsent     = "-"
)

func hashWork(root, rel string) string {
	if strings.HasSuffix(rel, "/") {
		return hashUnreadable
	}
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return hashUnreadable
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func hashIndex(root, rel string) string {
	out, err := gitprobe.Command(root, "rev-parse", "--verify", "-q", ":"+rel).CombinedOutput()
	if err != nil {
		// -q leaves stdout empty and writes nothing when the revision is
		// simply absent, so an empty result is "not in the index". Anything
		// else — git missing, the index unreadable — fails closed.
		if len(bytes.TrimSpace(out)) == 0 {
			return hashAbsent
		}
		return hashUnreadable
	}
	return strings.TrimSpace(string(out))
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
	keepMap := func(in map[string]string) map[string]string {
		if len(in) == 0 {
			return nil
		}
		out := map[string]string{}
		for k, v := range in {
			if !drop[k] {
				out[k] = v
			}
		}
		return out
	}
	return &Baseline{
		Root: b.Root, Tracked: keep(b.Tracked), Untracked: keep(b.Untracked), Staged: keep(b.Staged),
		Work: keepMap(b.Work), Index: keepMap(b.Index),
	}
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
	// The variable names are case-sensitive; git config keys are not, so
	// core.worktree is matched without regard to case.
	// CDPATH steers a relative cd somewhere other than where chdir looks, and
	// it can be set on the line by an assignment, read, mapfile or printf -v.
	envMoved := strings.Contains(strings.ToLower(cmd), "core.worktree") || strings.Contains(cmd, "CDPATH")
	// The raw text misses a name split by quotes (GIT''_DIR, CD''PATH), which
	// bash joins before the command sees it, so the words as bash passes them
	// are checked too.
	texts := []string{cmd}
	for _, c := range a.Commands {
		texts = append(texts, c.NameWord.Text)
		for _, w := range c.ArgWords {
			texts = append(texts, w.Text)
		}
	}
	for _, text := range texts {
		envMoved = envMoved || strings.Contains(strings.ToLower(text), "core.worktree") || strings.Contains(text, "CDPATH")
		for _, moved := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
			envMoved = envMoved || strings.Contains(text, moved)
		}
	}
	// dir is where the next command runs, while known. pending is the chain
	// whose cd set it without being sure to run: once past that chain, dir
	// is no longer known.
	dir, known, pending := cwd, cwdKnown, -1
	// varsSet is whether a builtin that can set a shell variable has run on
	// the line. Any of them may have set CDPATH — under a name the text check
	// above cannot see, like CD''PATH — so a relative cd after one is not
	// followed.
	varsSet := false
	for _, c := range a.Commands {
		if pending >= 0 && c.Chain != pending {
			known, pending = false, -1
		}
		if payload, ok := bashparser.ShellPayload(c.Name, c.Args); ok {
			if hits, what, stage := b.scan(payload, dir, known, depth+1); len(hits) > 0 {
				return hits, what, stage
			}
			if bashparser.Base(c.Name) == "eval" {
				// eval runs its payload in this shell: a cd there moves
				// the shell the rest of the line runs in.
				known, pending = false, -1
			}
			continue
		}
		name, args, ok := trust.Unwrapped(c)
		if !ok {
			// A command whose name is an expansion may be cd itself.
			known, pending = false, -1
			continue
		}
		words := c.ArgWords[len(c.ArgWords)-len(args):]
		switch bashparser.Base(name) {
		case "cd", "pushd", "popd":
			dir, known, pending = b.chdir(a, c, words, dir, known, varsSet)
			continue
		case "read", "declare", "typeset", "export", "local", "readonly", "let", "getopts":
			varsSet = true
			continue
		case "printf":
			for _, a := range args {
				if strings.HasPrefix(a, "-v") || !literalWord(words, a) {
					varsSet = true
				}
			}
			continue
		case "eval", "source", ".", "builtin", "command", "mapfile", "readarray", "trap", "enable", "alias":
			// These run code in this shell — a sourced file, an eval'd
			// string, a mapfile -C callback, a trap — or rename what a
			// later word runs, so where the shell is afterwards is not
			// something this reading can follow.
			known, pending = false, -1
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
		if d.index && len(d.paths) == 0 {
			// A pathless commit records the index whatever the line's
			// paths: what was staged before the run is in reach.
			hits = at.base.stillStaged()
		} else if d.all || viaXargs || !literal || !pathsKnown {
			hits = at.base.pick(d, nil, true)
		} else {
			hits = at.base.pick(d, at.base.resolve(d.paths, at.dir), len(d.paths) == 0)
		}
		if d.stage && !(d.index && len(d.paths) == 0) {
			hits = at.base.stillBaseline(hits)
		}
		if d.index && len(d.paths) > 0 {
			// -i/--include commits the pathspecs and the index together,
			// so the paths staged at capture are in reach as well.
			hits = union(hits, at.base.stillStaged())
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
func (b *Baseline) chdir(a bashparser.Analysis, c bashparser.Command, words []bashparser.Word, dir string, known, varsSet bool) (string, bool, int) {
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
		// bash searches an inherited CDPATH before the current directory
		// for a relative name that does not start with ./ or ../.
		// So does one set earlier on the line.
		explicit := strings.HasPrefix(t, "./") || strings.HasPrefix(t, "../") || t == "." || t == ".."
		if !explicit && (varsSet || os.Getenv("CDPATH") != "") {
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

// literalWord reports whether the argument text a came from a literal word.
func literalWord(words []bashparser.Word, a string) bool {
	for _, w := range words {
		if w.Text == a {
			return w.Literal
		}
	}
	return false
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
	index     bool     // also commits the index as it stands, not only the pathspecs
	untracked bool     // can remove untracked files
	flags     []string // for the message
}

// discard classifies one git subcommand. It returns nil when the invocation
// cannot discard uncommitted work.
func discard(sub string, args []string) *discardSpec {
	// A short-option cluster ending in a valued option takes the next word
	// as that value: `git commit -qm wip` has no pathspec. Only where the
	// letter always takes a value; `checkout -m` is a flag.
	clustered := ""
	switch sub {
	case "commit":
		clustered = "mFCc"
	case "stash":
		clustered = "m"
	}
	flags, paths, dashdash := splitArgs(args, clustered)
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
			f2, p2, _ := splitArgs(rest, clustered)
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
			// Commits what is staged. An add by the run was checked when it
			// ran; a path staged before the run began was not (WI-1596).
			return &discardSpec{tracked: true, stage: true, index: true}
		}
		// -i/--include records the named pathspecs and the index as it
		// stands, so pre-staged work rides along (WI-1596).
		if has("-i", "--include") {
			return &discardSpec{paths: paths, tracked: true, stage: true, index: true, flags: []string{"--include"}}
		}
		return &discardSpec{paths: paths, tracked: true, stage: true}
	}
	return nil
}

// splitArgs separates flags from positional arguments. Everything after `--`
// is positional. Flags that take a separate value consume it.
// clustered names the short options that take a value even at the end of a
// cluster like -qm.
func splitArgs(args []string, clustered string) (flags, positional []string, dashdash bool) {
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
			cluster := len(a) > 2 && a[1] != '-' && strings.IndexByte(clustered, a[len(a)-1]) >= 0
			if (valued[a] || cluster) && i+1 < len(args) {
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

// stillBaseline drops staging hits the session may record: a path whose
// work-tree or index bytes differ from the hash taken at capture, and a
// path git currently has unmerged because the session is resolving a merge.
// A path with no stored hash, or whose bytes cannot be re-read, stays.
func (b *Baseline) stillBaseline(hits []string) []string {
	if len(hits) == 0 {
		return nil
	}
	merging := b.unmerged()
	var keep []string
	for _, p := range hits {
		if merging[strings.TrimSuffix(p, "/")] {
			continue
		}
		if b.edited(p) {
			continue
		}
		keep = append(keep, p)
	}
	return keep
}

// stillStaged returns the paths staged at capture that a plain commit would
// still record as they were: the index entry is unchanged since capture, and
// git does not have the path unmerged. A path the session has restaged,
// unstaged or removed from the index has a different index entry and is the
// session's to commit. A path whose captured entry could not be read stays,
// and a path whose current index entry cannot be read stays too: failing
// to read the index is a refusal, not a pass (WI-1596).
func (b *Baseline) stillStaged() []string {
	if len(b.Staged) == 0 {
		return nil
	}
	merging := b.unmerged()
	var keep []string
	for _, p := range b.Staged {
		if merging[p] {
			continue
		}
		want, ok := b.Index[p]
		if !ok || want == hashUnreadable {
			keep = append(keep, p)
			continue
		}
		got := hashIndex(b.Root, p)
		switch got {
		case hashAbsent:
			// The session took it out of the index.
		case hashUnreadable:
			keep = append(keep, p)
		default:
			if got == want {
				keep = append(keep, p)
			}
		}
	}
	return keep
}

// edited reports whether the path's work-tree bytes or index blob differ
// from the baseline. Missing hashes and unreadable content are not edits.
func (b *Baseline) edited(p string) bool {
	if len(b.Work) == 0 && len(b.Index) == 0 {
		return false
	}
	if want, ok := b.Work[p]; ok && want != hashUnreadable {
		if got := hashWork(b.Root, p); got != hashUnreadable && got != want {
			return true
		}
	}
	if want, ok := b.Index[p]; ok && want != hashUnreadable {
		if got := hashIndex(b.Root, p); got != hashUnreadable && got != want {
			return true
		}
	}
	return false
}

// unmerged is the set of paths with an unmerged index stage, which is how
// git records a file the session is resolving in a merge or a cherry-pick.
// An unreadable index protects everything: the set comes back empty and
// stillBaseline keeps the hits.
func (b *Baseline) unmerged() map[string]bool {
	out, err := gitprobe.Command(b.Root, "ls-files", "-u", "-z").Output()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, f := range bytes.Split(out, []byte{0}) {
		// "<mode> <object> <stage>\t<path>"
		line := string(f)
		tab := strings.IndexByte(line, '\t')
		if tab < 0 || tab+1 >= len(line) {
			continue
		}
		set[line[tab+1:]] = true
	}
	return set
}

// union appends the members of extra that are not already in hits, in order.
func union(hits, extra []string) []string {
	seen := map[string]bool{}
	for _, p := range hits {
		seen[p] = true
	}
	for _, p := range extra {
		if !seen[p] {
			hits = append(hits, p)
			seen[p] = true
		}
	}
	return hits
}

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
		"`git add <the files you changed>` (not -A, ., -u or commit -a), then "+
		"`git commit -m <message> -- <those files>`, leaving out the files in that list.",
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
