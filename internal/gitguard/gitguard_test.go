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
