package gitguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo builds a repository with committed files, then dirties some of them.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	for _, f := range []string{"README.md", "src/a.py", "src/b.py", "docs/x.md", "old.txt"} {
		write(t, dir, f, "committed\n")
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	write(t, dir, "src/a.py", "user edit\n")  // modified
	write(t, dir, "docs/x.md", "user edit\n") // modified, then staged
	git(t, dir, "add", "docs/x.md")
	git(t, dir, "mv", "old.txt", "new.txt")    // renamed
	write(t, dir, "scratch/notes.txt", "mine") // untracked directory
	write(t, dir, "todo.txt", "mine")          // untracked file
	return dir
}

func TestCapture(t *testing.T) {
	dir := repo(t)
	b, err := Capture(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"docs/x.md", "new.txt", "old.txt", "src/a.py"}; !reflect.DeepEqual(b.Tracked, want) {
		t.Errorf("Tracked = %v, want %v", b.Tracked, want)
	}
	if want := []string{"scratch/", "todo.txt"}; !reflect.DeepEqual(b.Untracked, want) {
		t.Errorf("Untracked = %v, want %v", b.Untracked, want)
	}
	if _, err := Capture(t.TempDir()); err != ErrNotRepo {
		t.Errorf("Capture outside a repo: err = %v, want ErrNotRepo", err)
	}
}

func TestWithout(t *testing.T) {
	b := &Baseline{Root: "/r", Tracked: []string{"a", "b"}, Untracked: []string{".klaudia/GOAL.md"}}
	got := b.Without("/r/.klaudia/GOAL.md", "/r/a", "/elsewhere/b")
	if !reflect.DeepEqual(got.Tracked, []string{"b"}) || len(got.Untracked) != 0 {
		t.Errorf("Without = %+v", got)
	}
}

func TestCheckCommand(t *testing.T) {
	b := &Baseline{
		Root:      "/r",
		Tracked:   []string{"README.md", "src/a.py"},
		Untracked: []string{"scratch/", "todo.txt"},
	}
	refuse := []string{
		// The command from #250, which reverted every listed file to HEAD.
		"git checkout -- AGENTS.md README.md docs/DISPATCH.md src/a.py && git status --short",
		"git checkout -- .",
		"git checkout -- src",
		"git checkout -- 'src/*.py'",
		"git checkout HEAD -- src/a.py",
		"git checkout README.md",
		"git checkout -f main",
		"git restore src/a.py",
		"git restore --worktree --staged src/a.py",
		"git restore .",
		"git reset --hard",
		"git reset --hard HEAD~1",
		"git clean -fd",
		"git clean -f scratch",
		"git stash",
		"git stash push src/a.py",
		"git stash -u",
		"git rm -f README.md",
		"git switch --discard-changes main",
		"git -C src checkout -- a.py",
		"cd src && git checkout -- b.py", // cd: paths unknowable
		"git checkout -- $FILES",         // expansion
		"echo README.md | xargs git checkout --",
		"sh -c 'git checkout -- README.md'",
		"bash -lc \"cd /r && git reset --hard\"",
		"sudo git checkout -- README.md",
		"git status; git checkout -- src/a.py",
		// Staging the user's changes into the run's commit (#250, operator scope).
		"git add -A && git commit -m wip",
		"git add .",
		"git add -u",
		"git add src/a.py",
		"git add scratch/notes.txt",
		"git commit -am wip",
		"git commit -a -m wip",
		"git commit -m wip src/a.py",
	}
	for _, c := range refuse {
		if msg := b.CheckCommand(c, "/r"); msg == "" {
			t.Errorf("CheckCommand(%q) allowed it; want a refusal", c)
		} else if !strings.Contains(msg, "before this run") {
			t.Errorf("CheckCommand(%q) = %q; want it to explain the pre-existing changes", c, msg)
		}
	}
	allow := []string{
		"git status --short",
		"git diff",
		"git log --oneline",
		"git add src/b.py docs/new.md && git commit -m 'own work'",
		"git commit -m wip",
		"git commit --amend --no-edit",
		"git add -n .",
		"git checkout -- src/b.py",       // only its own file
		"git checkout -- docs/other.md",  // only its own file
		"git restore --staged src/a.py",  // unstages only
		"git checkout -b klaudia/goal-x", // new branch keeps the tree
		"git switch main",                // git refuses over local changes
		"git reset HEAD~1",               // mixed reset keeps the tree
		"git reset --soft HEAD~1",
		"git clean -n",
		"git clean -f build", // untracked, but not protected
		"git stash list",
		"git stash show -p",
		"git rm --cached -f README.md",
		"git revert HEAD",
		"rm -rf build",
		"go test ./...",
		"echo 'git checkout -- README.md'",
		"git checkout -- /elsewhere/README.md",
	}
	for _, c := range allow {
		if msg := b.CheckCommand(c, "/r"); msg != "" {
			t.Errorf("CheckCommand(%q) refused: %s", c, msg)
		}
	}
}

// A clean tree protects nothing, so the loop's own reverts are untouched.
func TestCheckCommandEmptyBaseline(t *testing.T) {
	b := &Baseline{Root: "/r"}
	for _, c := range []string{"git reset --hard", "git checkout -- .", "git clean -fdx"} {
		if msg := b.CheckCommand(c, "/r"); msg != "" {
			t.Errorf("empty baseline refused %q: %s", c, msg)
		}
	}
	var nilB *Baseline
	if msg := nilB.CheckTool("Bash", []byte(`{"command":"git reset --hard"}`), "/r"); msg != "" {
		t.Errorf("nil baseline refused: %s", msg)
	}
}

func TestCheckTool(t *testing.T) {
	b := &Baseline{Root: "/r", Tracked: []string{"a.go"}}
	if msg := b.CheckTool("Bash", []byte(`{"command":"git checkout -- a.go"}`), "/r"); msg == "" {
		t.Error("Bash discard over a protected path was allowed")
	}
	if msg := b.CheckTool("Read", []byte(`{"file_path":"/r/a.go"}`), "/r"); msg != "" {
		t.Errorf("non-Bash tool refused: %s", msg)
	}
}

// Against a real repository: the refusal is the difference between the user's
// edit surviving and not.
func TestGuardKeepsUserEditInRealRepo(t *testing.T) {
	dir := repo(t)
	b, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := "git checkout -- README.md src/a.py src/b.py"
	if msg := b.CheckCommand(cmd, dir); msg == "" {
		t.Fatalf("guard allowed %q over the user's src/a.py", cmd)
	}
	// The same command scoped to a file the user had not touched is allowed.
	if msg := b.CheckCommand("git checkout -- src/b.py", dir); msg != "" {
		t.Fatalf("guard refused a revert of an unprotected file: %s", msg)
	}
}

// clean builds a repository with one commit and nothing uncommitted.
func clean(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, dir, "main.go", "package main\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	return dir
}

// The start repository's baseline applies to commands in it, not to commands
// aimed at another repository or a linked worktree (#289).
func TestGuardFollowsTargetRepo(t *testing.T) {
	a := repo(t) // the start repository, with the user's changes
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	// The run's own files, written through the guard as the Write tool would.
	own := func(dir, rel string) {
		t.Helper()
		in := []byte(`{"file_path":"` + filepath.Join(dir, rel) + `"}`)
		if msg := b.CheckTool("Write", in, a); msg != "" {
			t.Fatalf("Write refused: %s", msg)
		}
		write(t, dir, rel, "the run's own work\n")
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a, "worktree", "add", "-q", "-b", "agent", wt)
	own(wt, "agent.go")
	other := clean(t)
	own(other, "new.go")
	nongit := t.TempDir()

	refuse := []struct{ cmd, cwd string }{
		{"git add -A", a},
		{"git -C " + a + " add -A", other},
		{"cd " + a + " && git add .", other},
		{"git --git-dir=" + a + "/.git --work-tree=" + a + " add -A", other},
		// Unreadable targets keep today's reading: the start repository.
		{`cd "$X" && git add -A`, other},
		{"false && cd " + other + "; git add -A", a},
		{"(cd " + other + " && true); git add -A", a},
		{"cd " + other + " | true; git add -A", a},
		{"cd /nonexistent-gitguard-dir; git add -A", a},
		{"GIT_DIR=" + other + "/.git git add -A", a},
		{"env -C " + other + " git add -A", a},
		{`git -C "$X" add -A`, other},
		{"sh -c 'cd " + a + " && git add -A'", other},
		// --git-dir and --work-tree keep the start repository's reading: the
		// index a command writes is the git dir's, not the work tree's (#293).
		{"git --git-dir=" + a + "/.git --work-tree=" + other + " add -A", other},
		{"git --git-dir=" + a + "/.git --work-tree=" + wt + " add -A", a},
		{"git --work-tree=" + other + " add -A", a},
		{"git --git-dir=" + a + "/.git reset --hard", other},
		{"git -C " + other + " --git-dir=" + a + "/.git add -A", other},
		{"git --git-dir=" + other + "/.git --work-tree=" + other + " add -A", a},
		// So does anything else that points git at the start repository's
		// index, refs or work tree from another tree.
		{"GIT_INDEX_FILE=" + a + "/.git/index git add -A", other},
		{"env GIT_INDEX_FILE=" + a + "/.git/index git add -A", other},
		{"GIT_COMMON_DIR=" + a + "/.git git add -A", other},
		{"git config core.worktree " + a + " && git checkout -- . && git clean -fdx", other},
		{"git config core.WorkTree " + a + " && git checkout -- . && git clean -fdx", other},
		{"git config CORE.WORKTREE " + a + " && git clean -fdx", other},
		{"git config --add core.Worktree " + a + " && git clean -fdx", other},
	}
	for _, c := range refuse {
		if msg := b.CheckCommand(c.cmd, c.cwd); msg == "" {
			t.Errorf("CheckCommand(%q) in %s allowed it; want a refusal", c.cmd, c.cwd)
		}
	}
	allow := []struct{ cmd, cwd string }{
		{"git -C " + wt + " add -A", a},
		{"cd " + wt + " && git add -A && git commit -m own", a},
		{"cd " + wt + "; git add .", a},
		{"git add -A", wt},
		{"cd " + other + " && git add .", a},
		{"git -C " + other + " add -A", a},
		{"sh -c 'cd " + other + " && git add -A'", a},
		{"git -C " + nongit + " add -A", a},
		{"cd " + nongit + " && git clean -fdx", a},
		{"git -C " + a + " checkout -- src/b.py", other},
	}
	for _, c := range allow {
		if msg := b.CheckCommand(c.cmd, c.cwd); msg != "" {
			t.Errorf("CheckCommand(%q) in %s refused: %s", c.cmd, c.cwd, msg)
		}
	}
}

// Another work tree's own pre-existing changes are protected too: its
// baseline is captured on the run's first touch, before that touch runs.
func TestGuardCapturesOtherWorktreeOnFirstTouch(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a, "worktree", "add", "-q", "-b", "user", wt)
	write(t, wt, "user-notes.txt", "the user's, before the run\n")

	// The first touch is the destructive command itself.
	msg := b.CheckCommand("git -C "+wt+" clean -f", a)
	if msg == "" || !strings.Contains(msg, "user-notes.txt") || !strings.Contains(msg, wt) {
		t.Fatalf("first touch of a dirty worktree: %q; want a refusal naming user-notes.txt in %s", msg, wt)
	}
	if msg := b.CheckCommand("cd "+wt+" && git add -A", a); msg == "" {
		t.Error("git add -A over the worktree's own untracked file was allowed")
	}
	// Its own new file, added by name, is fine.
	write(t, wt, "agent.go", "the run's\n")
	if msg := b.CheckCommand("git add agent.go", wt); msg != "" {
		t.Errorf("adding the run's own file was refused: %s", msg)
	}

	// A file the run writes before it ever runs git in a tree is the run's:
	// the write is the first touch, and it captures the tree clean.
	wt2 := filepath.Join(t.TempDir(), "wt2")
	git(t, a, "worktree", "add", "-q", "-b", "agent2", wt2)
	in := []byte(`{"file_path":"` + filepath.Join(wt2, "pkg", "new.go") + `"}`)
	if msg := b.CheckTool("Write", in, a); msg != "" {
		t.Fatalf("Write refused: %s", msg)
	}
	write(t, wt2, "pkg/new.go", "the run's\n")
	if msg := b.CheckCommand("git -C "+wt2+" add -A", a); msg != "" {
		t.Errorf("git add -A of the run's own files was refused: %s", msg)
	}
}

// A symlink to the start repository is the start repository, and relative
// paths are read against where the link points.
func TestGuardSymlinkIsSameRepo(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(a, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	other := clean(t)
	for _, c := range []struct{ cmd, cwd string }{
		{"git -C " + link + " add -A", other},
		{"git add -A", link},
		{"cd " + link + "/src && git checkout -- a.py", other},
		{"git -C " + link + " checkout -- src/a.py", other},
	} {
		if msg := b.CheckCommand(c.cmd, c.cwd); msg == "" {
			t.Errorf("CheckCommand(%q) in %s allowed it through a symlink", c.cmd, c.cwd)
		}
	}
	if msg := b.CheckCommand("git -C "+link+" checkout -- src/b.py", other); msg != "" {
		t.Errorf("an unprotected file through a symlink was refused: %s", msg)
	}
}

// A clean start repository still guards the other trees the run touches.
func TestGuardFromCleanStart(t *testing.T) {
	b, err := Capture(clean(t))
	if err != nil {
		t.Fatal(err)
	}
	guard := b.Guard()
	if guard == nil {
		t.Fatal("clean start: no guard")
	}
	dirty := repo(t)
	if msg := guard("Bash", []byte(`{"command":"git add -A"}`), dirty); msg == "" {
		t.Error("git add -A in a dirty repository touched for the first time was allowed")
	}
}

// Touches of a new repository from parallel calls all see one baseline,
// taken before any of them was allowed.
func TestGuardConcurrentFirstTouch(t *testing.T) {
	b, err := Capture(clean(t))
	if err != nil {
		t.Fatal(err)
	}
	dirty := repo(t)
	const n = 8
	msgs := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() { msgs <- b.CheckCommand("git -C "+dirty+" reset --hard", "/") }()
	}
	for i := 0; i < n; i++ {
		if msg := <-msgs; msg == "" {
			t.Error("a concurrent first touch was allowed to reset --hard")
		}
	}
}

// A work tree whose state cannot be read is not left unprotected: the command
// is judged as an unreadable target is, against the start repository with
// every protected path in reach.
func TestGuardFailsClosedWhenCaptureFails(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	broken := clean(t)
	// rev-parse still finds the work tree; status cannot read it.
	if err := os.WriteFile(filepath.Join(broken, ".git", "index"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(broken); err == nil {
		t.Fatal("Capture of a repository with a corrupt index succeeded; the test needs it to fail")
	}
	msg := b.CheckCommand("git -C "+broken+" add -A", a)
	if msg == "" {
		t.Fatal("a command in a work tree that could not be captured was allowed")
	}
	if !strings.Contains(msg, "todo.txt") {
		t.Errorf("refusal does not judge against the start repository's paths: %s", msg)
	}
	// A clean start repository has nothing to protect, so it stays allowed.
	c, err := Capture(clean(t))
	if err != nil {
		t.Fatal(err)
	}
	if msg := c.CheckCommand("git -C "+broken+" add -A", a); msg != "" {
		t.Errorf("clean start refused: %s", msg)
	}
}

// The touch every Bash call makes asks git once per directory, not per call;
// a directory that is not a work tree is asked again, since it may become one.
func TestProbeIsCachedForWorkTreesOnly(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	real := runProbe
	runProbe = func(dir string, gitOpts []string) (string, string, string, error) {
		calls[dir]++
		return real(dir, gitOpts)
	}
	t.Cleanup(func() { runProbe = real })

	other := clean(t)
	plain := t.TempDir()
	for i := 0; i < 3; i++ {
		b.CheckCommand("ls", other)
		b.CheckCommand("ls", plain)
	}
	if n := calls[other]; n != 1 {
		t.Errorf("work tree probed %d times over 3 calls, want 1", n)
	}
	if n := calls[plain]; n != 3 {
		t.Errorf("plain directory probed %d times over 3 calls, want 3 (a miss must not be cached)", n)
	}
	// A plain directory that becomes a dirty work tree is protected from then on.
	git(t, plain, "init", "-q")
	write(t, plain, "users.txt", "the user's\n")
	if msg := b.CheckCommand("git add -A", plain); msg == "" {
		t.Error("a directory that became a dirty work tree was left unprotected")
	}
}

// A cached baseline is trusted only while the tree is still the tree it was
// captured for. After the run has touched a clean other tree — so both the
// probe and the baseline are cached — swapping that tree's .git for a
// `gitdir:` file that points at the start repository, or writing
// core.worktree into its config, must not let `git add -A` through on the
// stale baseline: both redirect the command at the start repository (#295).
func TestGuardDropsBaselineWhenGitDirChanges(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		name string
		swap func(other string)
	}{
		{"gitdir file swap", func(other string) {
			if err := os.RemoveAll(filepath.Join(other, ".git")); err != nil {
				t.Fatal(err)
			}
			gitdir := "gitdir: " + filepath.Join(a, ".git") + "\n"
			if err := os.WriteFile(filepath.Join(other, ".git"), []byte(gitdir), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"core.worktree config", func(other string) {
			git(t, other, "config", "core.worktree", a)
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			other := clean(t)
			// The first touch caches a clean baseline and its git dir.
			if msg := b.CheckCommand("git status", other); msg != "" {
				t.Fatalf("first touch of a clean tree was refused: %s", msg)
			}
			route.swap(other)
			if msg := b.CheckCommand("cd "+other+" && git add -A", a); msg == "" {
				t.Errorf("git add -A was allowed after %s; the cached baseline was reused", route.name)
			}
		})
	}
}

// The identity belongs to the baseline, keyed by the tree's top, so it holds
// whichever directory the command was aimed at. A first touch at the top and a
// destructive command run from a subdirectory — or the reverse — must still be
// refused after the tree's .git is swapped for a gitdir file (#295).
func TestGuardIdentityHoldsAcrossDirectories(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ name, touch, attack string }{
		{"touch at top, attack from subdir", "", "sub"},
		{"touch in subdir, attack from top", "sub", ""},
	} {
		t.Run(route.name, func(t *testing.T) {
			other := clean(t)
			if err := os.Mkdir(filepath.Join(other, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			if msg := b.CheckCommand("git status", filepath.Join(other, route.touch)); msg != "" {
				t.Fatalf("first touch was refused: %s", msg)
			}
			if err := os.RemoveAll(filepath.Join(other, ".git")); err != nil {
				t.Fatal(err)
			}
			gitdir := "gitdir: " + filepath.Join(a, ".git") + "\n"
			if err := os.WriteFile(filepath.Join(other, ".git"), []byte(gitdir), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := "cd " + filepath.Join(other, route.attack) + " && git add -A"
			if msg := b.CheckCommand(cmd, a); msg == "" {
				t.Errorf("%s was allowed; the baseline cached for a different directory was reused", cmd)
			}
		})
	}
}

// Ordinary git work must not drop the baseline. status, add and commit rename
// files inside the .git directory, which changes its mtime, and a baseline
// re-captured after that would count files the run wrote as the user's. The
// identity is the .git entry's type and inode plus its config, so a file the
// run writes, then a status, then an add and commit, is the run's to commit.
func TestGuardKeepsBaselineAcrossGitActivity(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	other := clean(t)
	if msg := b.CheckCommand("git status --short", other); msg != "" {
		t.Fatalf("status in a clean tree was refused: %s", msg)
	}
	// Written after the first touch, so it is the run's, and the status above
	// has already renamed files inside .git.
	write(t, other, "one.go", "package one\n")
	if msg := b.CheckCommand("git add -A && git commit -qm c", other); msg != "" {
		t.Errorf("committing the run's own file was refused after a status: %s", msg)
	}
}

// A cached probe is trusted only while the directory still belongs to the tree
// it names. After the run has probed a subdirectory, a .git entry written there
// — a fresh repository, or a gitdir file pointing at the start repository —
// makes git run in that subdirectory target a different tree, while the cached
// top's own .git is unchanged. The command must be judged by the tree it now
// lands in, not by the cached answer (#295).
func TestGuardReprobesWhenGitDirAppearsBelow(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		name  string
		plant func(sub string)
	}{
		{"git init in the subdirectory", func(sub string) {
			git(t, sub, "init", "-q")
			write(t, sub, "secret.txt", "the user's\n")
		}},
		{"gitdir file pointing at the start repository", func(sub string) {
			gitdir := "gitdir: " + filepath.Join(a, ".git") + "\n"
			if err := os.WriteFile(filepath.Join(sub, ".git"), []byte(gitdir), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			other := clean(t)
			sub := filepath.Join(other, "sub")
			if err := os.Mkdir(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			// The first touch caches the probe: sub belongs to other.
			if msg := b.CheckCommand("git status", sub); msg != "" {
				t.Fatalf("first touch of a subdirectory was refused: %s", msg)
			}
			route.plant(sub)
			if msg := b.CheckCommand("cd "+sub+" && git add -A", a); msg == "" {
				t.Errorf("git add -A was allowed after %s; the probe cached before it appeared was reused", route.name)
			}
		})
	}
}

// A linked worktree of the start repository is judged by its own tree, so an
// explicit add there is not refused over the start repository's untracked files
// (#296). The worktree is clean apart from the one file being added.
func TestGuardAllowsExplicitAddInLinkedWorktree(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a, "worktree", "add", "-q", wt, "HEAD")
	// The run touches the worktree before it writes, as a Bash call into it
	// would, so the file it writes afterwards is its own.
	if msg := b.CheckCommand("git status", wt); msg != "" {
		t.Fatalf("first touch of a clean worktree was refused: %s", msg)
	}
	write(t, wt, "one.go", "package one\n")
	cmd := "git -C " + wt + " add -- one.go"
	if msg := b.CheckCommand(cmd, a); msg != "" {
		t.Errorf("explicit add in a linked worktree was refused: %s", msg)
	}
}

// A commit message carried as a quoted heredoc inside a command substitution is
// still a commit in the tree the line names. A literal leading cd into a linked
// worktree, then add and commit, must be judged by that worktree (#296).
func TestGuardFollowsLeadingCdBeforeHeredoc(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a, "worktree", "add", "-q", wt, "HEAD")
	if msg := b.CheckCommand("git status", wt); msg != "" {
		t.Fatalf("first touch of a clean worktree was refused: %s", msg)
	}
	write(t, wt, "one.go", "package one\n")
	cmd := "cd " + wt + " && git add -- one.go && git commit -m \"$(cat <<'EOF'\nsubject\n\nbody\nEOF\n)\""
	if msg := b.CheckCommand(cmd, a); msg != "" {
		t.Errorf("add and commit after a literal cd was refused: %s", msg)
	}
}

// A cd on the far side of a pipe does not move the shell that runs the git
// after it, so the git is judged where the line started. A cd that comes back
// before the git does move it (#296).
func TestGuardFollowsCdAcrossPipes(t *testing.T) {
	a := repo(t)
	b, err := Capture(a)
	if err != nil {
		t.Fatal(err)
	}
	other := clean(t)
	if msg := b.CheckCommand("git status", other); msg != "" {
		t.Fatalf("first touch of a clean tree was refused: %s", msg)
	}
	// The pipe's cd never runs in this shell: git add -A runs in the start
	// repository and must be refused.
	piped := "cd " + other + " | true && git add -A"
	if msg := b.CheckCommand(piped, a); msg == "" {
		t.Error("git add -A after a piped cd was allowed; the cd was taken to move this shell")
	}
	// Coming back before the git lands it in the clean tree, whose own file is
	// the run's to commit.
	back := "cd " + other + " | true && cd .. && cd " + other + " && git add -A && git commit -qm c"
	write(t, other, "one.go", "package one\n")
	if msg := b.CheckCommand(back, a); msg != "" {
		t.Errorf("add and commit after cd .. && cd back was refused: %s", msg)
	}
}
