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
// The reading is deliberately conservative. When a command's paths cannot be
// read — an expansion, a `cd` earlier in the line, paths arriving through
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
// that would discard protected changes, and "" for anything else.
func (b *Baseline) CheckTool(tool string, input []byte, cwd string) string {
	if b.Empty() || tool != "Bash" {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil || strings.TrimSpace(in.Command) == "" {
		return ""
	}
	return b.CheckCommand(in.Command, cwd)
}

// CheckCommand returns why cmd may not run, or "" when it may.
func (b *Baseline) CheckCommand(cmd, cwd string) string {
	if b.Empty() {
		return ""
	}
	hits, what := b.scan(cmd, cwd, 0)
	if len(hits) == 0 {
		return ""
	}
	return refusal(what, hits)
}

// maxDepth bounds how far nested `sh -c` payloads are followed.
const maxDepth = 4

// scan returns the protected paths cmd could discard and the git invocation
// that would do it.
func (b *Baseline) scan(cmd, cwd string, depth int) ([]string, string) {
	a, err := bashparser.Parse(cmd)
	if err != nil || depth > maxDepth {
		// Unreadable: refuse only if it plausibly runs a discarding git command.
		if mentionsDiscard(cmd) {
			return b.all(), "an unparsable command that mentions a discarding git subcommand"
		}
		return nil, ""
	}
	// After a cd the reader no longer knows where relative paths point.
	moved := false
	for _, c := range a.Commands {
		if payload, ok := bashparser.ShellPayload(c.Name, c.Args); ok {
			if hits, what := b.scan(payload, cwd, depth+1); len(hits) > 0 {
				return hits, what
			}
			continue
		}
		name, args, ok := trust.Unwrapped(c)
		if !ok {
			continue
		}
		switch bashparser.Base(name) {
		case "cd", "pushd", "popd":
			moved = true
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
		dir := cwd
		args, dir, dirKnown := globalOpts(args, dir)
		if len(args) == 0 {
			continue
		}
		d := discard(args[0], args[1:])
		if d == nil {
			continue
		}
		var hits []string
		if d.all || viaXargs || !literal || moved || !dirKnown {
			hits = b.pick(d, nil, true)
		} else {
			hits = b.pick(d, b.resolve(d.paths, dir), len(d.paths) == 0)
		}
		if len(hits) > 0 {
			return hits, "git " + strings.Join(append([]string{args[0]}, d.flags...), " ")
		}
	}
	return nil, ""
}

// discardSpec describes what a git subcommand invocation can throw away.
type discardSpec struct {
	paths     []string // pathspecs, as written
	all       bool     // reaches the whole tree whatever the pathspecs say
	tracked   bool     // can discard changes to tracked files
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
	}
	return nil
}

// splitArgs separates flags from positional arguments. Everything after `--`
// is positional. Flags that take a separate value consume it.
func splitArgs(args []string) (flags, positional []string, dashdash bool) {
	valued := map[string]bool{"-s": true, "--source": true, "-m": true, "--message": true,
		"-b": true, "-B": true, "--orphan": true, "-e": true, "--exclude": true,
		"--pathspec-from-file": true, "--conflict": true}
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
// subcommand runs in. dirKnown is false when -C names an unreadable directory.
func globalOpts(args []string, dir string) ([]string, string, bool) {
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "-C" && len(args) > 1:
			d := args[1]
			if !filepath.IsAbs(d) {
				d = filepath.Join(dir, d)
			}
			dir, args = d, args[2:]
		case (a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace") && len(args) > 1:
			if a == "--work-tree" || a == "--git-dir" {
				return args[2:], dir, false
			}
			args = args[2:]
		case strings.HasPrefix(a, "--work-tree=") || strings.HasPrefix(a, "--git-dir="):
			return args[1:], dir, false
		case strings.HasPrefix(a, "-"):
			args = args[1:]
		default:
			return args, dir, true
		}
	}
	return nil, dir, true
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
	for _, w := range []string{"checkout", "restore", "reset", "clean", "stash", " rm ", "switch"} {
		if strings.Contains(cmd, w) {
			return true
		}
	}
	return false
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
