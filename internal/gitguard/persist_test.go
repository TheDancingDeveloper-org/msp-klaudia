package gitguard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBaselineRoundTripIncludesOthers(t *testing.T) {
	dir := t.TempDir()
	b := &Baseline{
		Root:      dir,
		Tracked:   []string{"keep.go"},
		Untracked: []string{"notes.txt"},
		others: map[string]*Baseline{
			"/other": {Root: "/other", Tracked: []string{"theirs.go"}},
		},
	}
	raw, err := MarshalBaseline(b)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalBaseline(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Root != dir || len(back.Tracked) != 1 || back.Tracked[0] != "keep.go" {
		t.Fatalf("primary baseline = %+v", back)
	}
	o := back.others["/other"]
	if o == nil || len(o.Tracked) != 1 || o.Tracked[0] != "theirs.go" {
		t.Fatalf("others = %+v", back.others)
	}
	if _, err := UnmarshalBaseline([]byte("{")); err == nil {
		t.Fatal("bad json accepted")
	}
	// The on-disk file is what a hook writes; keep the shape obvious.
	if filepath.Separator == '/' && !os.IsPathSeparator(raw[0]) && raw[0] != '{' {
		t.Fatalf("marshal = %s", raw)
	}
}

// reload round-trips b through its file, as a resumed session (or a hook
// process per call) sees it.
func reload(t *testing.T, b *Baseline, path string) *Baseline {
	t.Helper()
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Resume(path, "/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func bash(c string) []byte {
	raw, _ := json.Marshal(map[string]string{"command": c})
	return raw
}

// WI-1300: the cwd is a dirty main checkout, the session works in a clean
// linked worktree, and the baseline is reloaded between calls — a resume, or
// a hook that runs once per call. The session's own edit to a path that is
// also dirty in the main checkout must commit; before the identity was
// persisted the reloaded worktree baseline was dropped and re-captured with
// that edit in it, and the commit was refused as pre-existing.
func TestReloadedBaselineCommitsOwnEditInSiblingWorktree(t *testing.T) {
	main := clean(t)
	write(t, main, "engine/budget.rs", "v1\n")
	git(t, main, "add", "-A")
	git(t, main, "commit", "-qm", "budget")
	write(t, main, "engine/budget.rs", "user dirt in the main checkout\n")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, main, "worktree", "add", "-q", "-b", "wi", wt, "HEAD")

	path := filepath.Join(t.TempDir(), "s.gitguard.json")
	b, err := Capture(main)
	if err != nil {
		t.Fatal(err)
	}
	edit, _ := json.Marshal(map[string]string{"file_path": filepath.Join(wt, "engine/budget.rs")})
	b.CheckTool("Edit", edit, main)
	b = reload(t, b, path)
	write(t, wt, "engine/budget.rs", "the session's edit\n")
	b = reload(t, b, path)
	for _, c := range []string{
		"cd " + wt + " && git add engine/budget.rs && git commit -qm x",
		"git -C " + wt + " add engine/budget.rs",
	} {
		if msg := b.CheckTool("Bash", bash(c), main); msg != "" {
			t.Errorf("%s: refused the session's own edit: %s", c, msg)
		}
		b = reload(t, b, path)
	}
	// The main checkout's own dirt is still the user's.
	if msg := b.CheckTool("Bash", bash("git add engine/budget.rs"), main); msg == "" {
		t.Error("staging the main checkout's pre-existing change was allowed")
	}
}

// A path that really was dirty in the target worktree when the session first
// touched it stays refused across a reload.
func TestReloadedBaselineStillRefusesWorktreeDirt(t *testing.T) {
	main := clean(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, main, "worktree", "add", "-q", "-b", "wi", wt, "HEAD")
	write(t, wt, "main.go", "package main // user dirt\n")
	b, err := Capture(main)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.gitguard.json")
	b.CheckTool("Bash", bash("cd "+wt+" && git status"), main)
	b = reload(t, b, path)
	for _, c := range []string{
		"cd " + wt + " && git add main.go",
		"cd " + wt + " && git checkout -- main.go",
		"git -C " + wt + " commit -am x",
	} {
		if msg := b.CheckTool("Bash", bash(c), main); msg == "" {
			t.Errorf("%s: allowed after a reload", c)
		}
	}
}

// Resume keeps the baseline the session first took: files the session wrote
// before a restart are its own, while the start dirt stays protected.
func TestResumeKeepsFirstBaseline(t *testing.T) {
	dir := repo(t)
	path := filepath.Join(t.TempDir(), "s.gitguard.json")
	b, err := Resume(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	guard := b.PersistentGuard(path)
	write(t, dir, "mine.go", "package mine\n") // the session's file
	guard("Bash", bash("git status"), dir)

	// Restart: a fresh Capture would record mine.go as the user's.
	if fresh, _ := Capture(dir); len(fresh.CheckCommand("git add mine.go", dir)) == 0 {
		t.Fatal("test premise: a fresh capture should protect mine.go")
	}
	again, err := Resume(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if msg := again.CheckCommand("git add mine.go", dir); msg != "" {
		t.Errorf("resumed session refused its own file: %s", msg)
	}
	if msg := again.CheckCommand("git add todo.txt", dir); msg == "" {
		t.Error("resumed session may stage the user's untracked file")
	}
	if msg := again.CheckCommand("git checkout -- src/a.py", dir); msg == "" {
		t.Error("resumed session may discard the user's edit")
	}
}

func TestResumeRefusesUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.gitguard.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resume(path, t.TempDir()); err == nil || errors.Is(err, ErrNotRepo) {
		t.Fatalf("Resume of a corrupt file = %v", err)
	}
}

// WI-1300 regression, the vogt-1316 case: a session starts in a dirty main
// checkout and edits files in a clean linked worktree of its own; the process
// is killed (an engine restart) and the session resumed; meanwhile another
// session's `git worktree add -b` rewrites the config the two worktrees share.
// The shared config's new ctime used to fail the reloaded baseline's identity,
// so it was dropped and re-captured with the session's edits in it, and the
// session was refused its own commit. A config write that leaves the tree
// resolving where it did must keep the baseline; dirt that was in the
// worktree at first touch, and a config that redirects the tree, stay refused.
func TestResumeAfterSharedConfigRewriteCommitsOwnEdits(t *testing.T) {
	main := clean(t)
	write(t, main, "notes.txt", "user dirt in the main checkout\n")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, main, "worktree", "add", "-q", "-b", "wi-1316", wt, "HEAD")
	write(t, wt, "dirty.go", "package main // there before the session\n")

	path := filepath.Join(t.TempDir(), "s.gitguard.json")
	b, err := Resume(path, main)
	if err != nil {
		t.Fatal(err)
	}
	guard := b.PersistentGuard(path)
	if msg := guard("Bash", bash("cd "+wt+" && git status --short"), main); msg != "" {
		t.Fatalf("first touch refused: %s", msg)
	}
	edit, _ := json.Marshal(map[string]string{"file_path": filepath.Join(wt, "main.go")})
	guard("Edit", edit, main)
	write(t, wt, "main.go", "package main // the session's edit\n")
	// The process dies here: nothing but the file survives.

	time.Sleep(20 * time.Millisecond)
	other := filepath.Join(t.TempDir(), "other")
	git(t, main, "worktree", "add", "-q", "-b", "wi-other", other, "HEAD")
	git(t, main, "config", "branch.wi-other.description", "another session")

	b, err = Resume(path, main)
	if err != nil {
		t.Fatal(err)
	}
	guard = b.PersistentGuard(path)
	if msg := guard("Bash", bash("cd "+wt+" && git add main.go && git commit -qm own"), main); msg != "" {
		t.Errorf("resumed session refused its own edit after a shared config rewrite: %s", msg)
	}
	if msg := guard("Bash", bash("cd "+wt+" && git add dirty.go"), main); msg == "" {
		t.Error("staging the worktree's pre-existing dirt was allowed after resume")
	}
	if msg := guard("Bash", bash("git add notes.txt"), main); msg == "" {
		t.Error("staging the main checkout's pre-existing dirt was allowed after resume")
	}

}

// redirectSetup is a dirty main checkout and a clean linked worktree with
// worktree-scoped config enabled, touched by a session that then writes a file
// of its own there, saved and reloaded as a resume would. own.go is in no
// baseline. notes.txt is dirt in the main checkout only: the worktree's
// baseline does not list it, so staging it from the worktree is refused only
// if the guard sees that the worktree now resolves onto the main checkout.
func redirectSetup(t *testing.T) (main, wt string, b *Baseline) {
	t.Helper()
	main = clean(t)
	write(t, main, "notes.txt", "user dirt in the main checkout\n")
	git(t, main, "config", "extensions.worktreeConfig", "true")
	wt = filepath.Join(t.TempDir(), "wt")
	git(t, main, "worktree", "add", "-q", "-b", "wi", wt, "HEAD")
	b, err := Capture(main)
	if err != nil {
		t.Fatal(err)
	}
	if msg := b.CheckTool("Bash", bash("cd "+wt+" && git status"), main); msg != "" {
		t.Fatalf("first touch refused: %s", msg)
	}
	write(t, wt, "own.go", "package main // the session's\n")
	b = reload(t, b, filepath.Join(t.TempDir(), "s.gitguard.json"))
	// The resumed session's first call into the worktree caches its probe,
	// so a later command there is judged by the remembered baseline.
	if msg := b.CheckTool("Bash", bash("cd "+wt+" && git add own.go"), main); msg != "" {
		t.Fatalf("resumed session refused its own file: %s", msg)
	}
	// Control: before any redirect the worktree has no notes.txt, so the
	// command the tests below expect refused is allowed here.
	if msg := b.CheckTool("Bash", bash("cd "+wt+" && git add notes.txt"), main); msg != "" {
		t.Fatalf("control: git add notes.txt in the unredirected worktree was refused: %s", msg)
	}
	return main, wt, b
}

// A worktree-scoped core.worktree really does move a linked worktree onto
// another tree (git honours it in config.worktree, not in the shared config).
// After a resume that write must drop the baseline, so `git add notes.txt`
// from the worktree, which would now stage the main checkout's dirt, is
// refused. The worktree's own baseline would have allowed it.
func TestResumeDropsBaselineOnRealRedirect(t *testing.T) {
	main, wt, b := redirectSetup(t)
	time.Sleep(20 * time.Millisecond)
	git(t, wt, "config", "--worktree", "core.worktree", main)
	if got := gitOut(t, wt, "rev-parse", "--show-toplevel"); canonical(got) != canonical(main) {
		t.Fatalf("setup: core.worktree did not redirect the worktree (toplevel %s)", got)
	}
	if msg := b.CheckTool("Bash", bash("cd "+wt+" && git add notes.txt"), main); msg == "" {
		t.Error("staging was allowed after core.worktree redirected the resumed tree")
	}
}

// The config stamps are taken before git is asked where the tree resolves and
// again after. A redirecting write that lands after git read the config, but
// before the second stamp, must drop the baseline, not refresh it.
func TestResumeRefusesConfigWrittenDuringProbe(t *testing.T) {
	main, wt, b := redirectSetup(t)
	// Move the stamps, harmlessly, so the reloaded identity takes the
	// re-probe path.
	time.Sleep(20 * time.Millisecond)
	git(t, main, "config", "branch.wi.description", "another session")

	real := runProbe
	t.Cleanup(func() { runProbe = real })
	injected := false
	runProbe = func(dir string, gitOpts []string) (string, string, string, error) {
		top, prefix, gitDir, err := real(dir, gitOpts)
		if !injected && canonical(dir) == canonical(wt) {
			injected = true
			time.Sleep(20 * time.Millisecond)
			git(t, wt, "config", "--worktree", "core.worktree", main)
		}
		return top, prefix, gitDir, err
	}
	// One lookup, so the check under test is the only one that can see the
	// write: a later lookup would catch it through the identity anyway.
	gitDir := gitOut(t, wt, "rev-parse", "--absolute-git-dir")
	if o := b.other(wt, gitDir); o != nil {
		t.Error("the baseline was kept across a redirect written during the probe")
	}
	if !injected {
		t.Fatal("the probe was never asked about the worktree; the race was not exercised")
	}
}
