package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gitAvailable skips a test on a machine without git, which the pure-Go build
// does not require.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// repo builds a repository with one commit and points the worktree root at a
// scratch config dir, so a test never writes to the developer's ~/.klaudia.
func repo(t *testing.T) string {
	t.Helper()
	gitAvailable(t)
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())

	root := t.TempDir()
	git(t, root, "init", "--quiet")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.com")
	write(t, root, "a.txt", "committed a\n")
	write(t, root, "b.txt", "committed b\n")
	write(t, root, ".gitignore", "*.log\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "--quiet", "-m", "first")
	return root
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), dir, nil, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

func TestSupported(t *testing.T) {
	gitAvailable(t)
	committed := repo(t)

	empty := t.TempDir()
	git(t, empty, "init", "--quiet")

	tests := []struct {
		name string
		root string
		want bool
	}{
		{name: "repository with a commit", root: committed, want: true},
		{name: "repository with no commit has no HEAD to check out", root: empty, want: false},
		{name: "plain directory", root: t.TempDir(), want: false},
		{name: "no root at all", root: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Supported(context.Background(), tt.root); got != tt.want {
				t.Errorf("Supported = %v, want %v", got, tt.want)
			}
		})
	}
}

// The bug this is here for: `git worktree add <dir> HEAD` hands the child the
// last commit, so the user's uncommitted work is simply absent. A child asked
// about the function the user just wrote would look at a tree without it and
// report, convincingly, that there is no such function.
func TestNewCarriesTheParentsUncommittedWork(t *testing.T) {
	root := repo(t)
	write(t, root, "a.txt", "edited but not committed\n")
	write(t, root, "new/untracked.txt", "created a minute ago\n")
	write(t, root, "build.log", "ignored build output\n")
	// Staged as well as unstaged: `git diff HEAD` has to cover both.
	write(t, root, "b.txt", "staged edit\n")
	git(t, root, "add", "b.txt")

	tree, err := New(context.Background(), root, "general-purpose")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Remove(context.Background())

	if got := read(t, tree.Dir, "a.txt"); got != "edited but not committed\n" {
		t.Errorf("unstaged edit missing from the checkout: %q", got)
	}
	if got := read(t, tree.Dir, "b.txt"); got != "staged edit\n" {
		t.Errorf("staged edit missing from the checkout: %q", got)
	}
	if got := read(t, tree.Dir, "new/untracked.txt"); got != "created a minute ago\n" {
		t.Errorf("untracked file missing from the checkout: %q", got)
	}
	if _, err := os.Stat(filepath.Join(tree.Dir, "build.log")); err == nil {
		t.Error("ignored file was copied; build output is what makes a copy expensive")
	}

	// The baseline must be this state, not HEAD: a child that changed nothing
	// must come back with nothing to adopt.
	rep, err := tree.Adopt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Empty() {
		t.Errorf("a child that wrote nothing reported changes: %+v", rep)
	}
}

// Seeding must not touch the parent's index: the staging area is the user's
// statement about what belongs in their next commit.
func TestNewLeavesTheParentsIndexAlone(t *testing.T) {
	root := repo(t)
	write(t, root, "a.txt", "edited\n")
	write(t, root, "b.txt", "also edited\n")
	git(t, root, "add", "a.txt")
	before := git(t, root, "diff", "--cached", "--name-only")

	tree, err := New(context.Background(), root, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Remove(context.Background())

	if after := git(t, root, "diff", "--cached", "--name-only"); after != before {
		t.Errorf("staged set changed from %q to %q", before, after)
	}
}

func TestAdoptBringsBackEveryKindOfChange(t *testing.T) {
	root := repo(t)
	ctx := context.Background()
	tree, err := New(ctx, root, "general-purpose")
	if err != nil {
		t.Fatal(err)
	}

	// What a child does: edit, create, delete.
	write(t, tree.Dir, "a.txt", "the child's edit\n")
	write(t, tree.Dir, "c.txt", "the child's new file\n")
	if err := os.Remove(filepath.Join(tree.Dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	// Build output the child produced along the way stays behind: it is
	// ignored, so it is not part of the work.
	write(t, tree.Dir, "noise.log", "build output\n")

	rep, err := tree.Adopt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conflicted) != 0 {
		t.Fatalf("unexpected conflicts: %v", rep.Conflicted)
	}
	want := map[string]bool{"a.txt": true, "b.txt": true, "c.txt": true}
	if len(rep.Adopted) != len(want) {
		t.Fatalf("adopted %v, want %v", rep.Adopted, want)
	}
	for _, p := range rep.Adopted {
		if !want[p] {
			t.Errorf("adopted unexpected path %q", p)
		}
	}
	if got := read(t, root, "a.txt"); got != "the child's edit\n" {
		t.Errorf("edit not applied: %q", got)
	}
	if got := read(t, root, "c.txt"); got != "the child's new file\n" {
		t.Errorf("new file not applied: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err == nil {
		t.Error("deletion not applied")
	}
	if _, err := os.Stat(filepath.Join(root, "noise.log")); err == nil {
		t.Error("ignored file was adopted")
	}
	// Applied to the working tree only.
	if staged := git(t, root, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("adoption staged %q; the index is the user's", staged)
	}
}

// The case the whole conflict path exists for: the user (or another child) edits
// a file while this one works. Overwriting silently is the bug; so is throwing
// away the three files that were fine.
func TestAdoptKeepsItsHandsOffFilesThatMovedUnderneath(t *testing.T) {
	root := repo(t)
	ctx := context.Background()
	tree, err := New(ctx, root, "general-purpose")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Remove(ctx)

	write(t, tree.Dir, "a.txt", "the child's edit\n")
	write(t, tree.Dir, "b.txt", "the child's other edit\n")
	write(t, root, "a.txt", "the user's edit, made meanwhile\n")

	rep, err := tree.Adopt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rep.Conflicted, ","); got != "a.txt" {
		t.Errorf("conflicted = %v, want [a.txt]", rep.Conflicted)
	}
	if got := strings.Join(rep.Adopted, ","); got != "b.txt" {
		t.Errorf("adopted = %v, want [b.txt]", rep.Adopted)
	}
	if got := read(t, root, "a.txt"); got != "the user's edit, made meanwhile\n" {
		t.Errorf("the user's edit was overwritten: %q", got)
	}
	if got := read(t, root, "b.txt"); got != "the child's other edit\n" {
		t.Errorf("a file that fit was discarded with the one that did not: %q", got)
	}
}

func TestRemoveLeavesNoTrace(t *testing.T) {
	root := repo(t)
	ctx := context.Background()
	tree, err := New(ctx, root, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tree.Dir); err == nil {
		t.Error("checkout directory still there")
	}
	if list := git(t, root, "worktree", "list"); strings.Contains(list, tree.Dir) {
		t.Errorf("git still lists the checkout:\n%s", list)
	}
}

func TestRewriteMapsCheckoutPathsBackToTheProject(t *testing.T) {
	tree := &Tree{Root: "/project", Dir: "/wt/agent-1", aliases: []string{"/wt/agent-1", "/private/wt/agent-1"}}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a finding the parent can follow",
			in:   "The bug is in /wt/agent-1/internal/api/client.go:42",
			want: "The bug is in /project/internal/api/client.go:42",
		},
		{
			name: "the symlink-resolved spelling a tool may report",
			in:   "wrote /private/wt/agent-1/main.go",
			want: "wrote /project/main.go",
		},
		{
			name: "text with no checkout path is untouched",
			in:   "nothing to see here",
			want: "nothing to see here",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tree.Rewrite(tt.in); got != tt.want {
				t.Errorf("Rewrite = %q, want %q", got, tt.want)
			}
		})
	}
	var nilTree *Tree
	if got := nilTree.Rewrite("x"); got != "x" {
		t.Errorf("nil Tree.Rewrite = %q", got)
	}
}

func TestReportSummary(t *testing.T) {
	tests := []struct {
		name string
		rep  Report
		want string
	}{
		{name: "nothing happened", rep: Report{}, want: "no file changes"},
		{
			name: "one file",
			rep:  Report{Adopted: []string{"a.txt"}},
			want: "1 file applied to the working tree",
		},
		{
			name: "several files",
			rep:  Report{Adopted: []string{"a.txt", "b.txt"}},
			want: "2 files applied to the working tree",
		},
		{
			name: "a conflict names the file, because the model has to say so",
			rep:  Report{Adopted: []string{"a.txt"}, Conflicted: []string{"b.txt"}},
			want: "1 file applied to the working tree; 1 file NOT applied (changed meanwhile): b.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rep.Summary(); got != tt.want {
				t.Errorf("Summary = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"general-purpose", "general-purpose"},
		{"Explore", "explore"},
		{"../../etc/passwd", "etc-passwd"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Two children finishing at once is the case the package is for, and the last
// step must not be the one that loses work: whichever applies second has to see
// the first's change and report a conflict rather than write over it.
func TestConcurrentAdoptionOfOneFilePicksAWinner(t *testing.T) {
	root := repo(t)
	ctx := context.Background()

	first, err := New(ctx, root, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(ctx, root, "two")
	if err != nil {
		t.Fatal(err)
	}
	write(t, first.Dir, "a.txt", "from the first child\n")
	write(t, second.Dir, "a.txt", "from the second child\n")

	reps := make([]Report, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, tree := range []*Tree{first, second} {
		wg.Add(1)
		go func(i int, tree *Tree) {
			defer wg.Done()
			reps[i], errs[i] = tree.Adopt(ctx)
		}(i, tree)
	}
	wg.Wait()
	defer first.Remove(ctx)
	defer second.Remove(ctx)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Adopt %d: %v", i, err)
		}
	}
	adopted := len(reps[0].Adopted) + len(reps[1].Adopted)
	conflicted := len(reps[0].Conflicted) + len(reps[1].Conflicted)
	if adopted != 1 || conflicted != 1 {
		t.Fatalf("adopted %d and conflicted %d, want exactly one of each: %+v", adopted, conflicted, reps)
	}
	// And the file holds one child's whole version, not a blend of the two.
	got := read(t, root, "a.txt")
	if got != "from the first child\n" && got != "from the second child\n" {
		t.Errorf("a.txt = %q, which is neither child's version", got)
	}
}
