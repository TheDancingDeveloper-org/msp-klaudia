package cli

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenthread-ai/klaudia/internal/session"
)

// gitRepo makes a git repository with a nested subdirectory and returns both.
func gitRepo(t *testing.T) (repo, sub string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo = t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	sub = filepath.Join(repo, "internal", "tui")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, sub
}

func TestProjectRootIsTheGitTopLevel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	repo, sub := gitRepo(t)
	if got := projectRoot(sub); got != repo {
		t.Errorf("projectRoot(subdir) = %q, want the top-level %q", got, repo)
	}
	if got := projectRoot(repo); got != repo {
		t.Errorf("projectRoot(top-level) = %q, want %q", got, repo)
	}
}

func TestProjectRootOutsideARepositoryIsCWD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if got := projectRoot(dir); got != dir {
		t.Errorf("projectRoot(non-repo) = %q, want cwd %q", got, dir)
	}
}

// A checkout reached through a symlink keeps the symlinked spelling, so the
// session dir of an existing repo-root launch does not move.
func TestProjectRootKeepsTheSymlinkedSpelling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	repo, _ := gitRepo(t)
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if got := projectRoot(filepath.Join(link, "internal", "tui")); got != link {
		t.Errorf("projectRoot via symlink = %q, want %q", got, link)
	}
	if got := projectRoot(link); got != link {
		t.Errorf("projectRoot(symlinked top-level) = %q, want %q", got, link)
	}
}

// A dotfiles repository in $HOME must not turn every directory below it into
// one project.
func TestProjectRootIgnoresARepositoryAtHome(t *testing.T) {
	repo, sub := gitRepo(t)
	t.Setenv("HOME", repo)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	if got := projectRoot(sub); got != sub {
		t.Errorf("projectRoot under a $HOME repo = %q, want cwd %q", got, sub)
	}
}

// A session recorded while sessions were keyed by the launch directory is
// still auto-resumed from that directory, and the newer of the two keys wins.
func TestResolveResumeIDReadsTheCWDKeyedSessionsToo(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	root, cwd := "/work/proj", "/work/proj/internal/tui"

	seedSession(t, cwd, "legacy-subdir-session")
	if got, err := resolveResumeID(root, cwd, options{}, true); err != nil || got != "legacy-subdir-session" {
		t.Fatalf("resolveResumeID = %q, %v; want the cwd-keyed session", got, err)
	}
	if got, err := resolveResumeID(root, cwd, options{continueSession: true}, false); err != nil || got != "legacy-subdir-session" {
		t.Fatalf("--continue = %q, %v; want the cwd-keyed session", got, err)
	}

	// Make the root-keyed session unambiguously newer.
	seedSession(t, root, "root-session")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(session.Path(root, "root-session"), future, future); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveResumeID(root, cwd, options{}, true); got != "root-session" {
		t.Fatalf("resolveResumeID = %q, want the newer root-keyed session", got)
	}
}

// End to end: a launch from a subdirectory of a repository records under the
// repository's session dir, continues the session started at the top, recalls
// the top-level memory (and the subdirectory's own, older memory), and still
// runs in the subdirectory.
func TestSubdirectoryLaunchSharesTheProjectState(t *testing.T) {
	fake := &fakeChat{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	cfgDir := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", cfgDir)

	repo, sub := gitRepo(t)
	cfg := fmt.Sprintf("provider = \"openai\"\nbaseURL = %q\napiKey = \"test\"\nmodel = \"fake-model\"\n", srv.URL)
	for _, d := range []string{repo, sub} {
		if err := os.MkdirAll(filepath.Join(d, ".klaudia"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, ".klaudia", "config.toml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".klaudia", "MEMORY.md"), []byte("- root-note-xyzzy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".klaudia", "MEMORY.md"), []byte("- subdir-note-plugh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := embed(t, repo, []string{"--session-id", "proj-1"}, "remember the word pineapple"); err != nil {
		t.Fatalf("top-level launch: %v", err)
	}
	lines, err := embed(t, sub, []string{"--continue"}, "what was the word?")
	if err != nil {
		t.Fatalf("subdirectory launch: %v", err)
	}
	if init := lines[0]; init["session_id"] != "proj-1" || init["resumed"] != true {
		t.Fatalf("subdirectory --continue init = %v, want proj-1 resumed", init)
	}
	body := fake.last()
	for _, want := range []string{"pineapple", "root-note-xyzzy", "subdir-note-plugh", "Working directory: " + sub} {
		if !strings.Contains(body, want) {
			t.Errorf("request from the subdirectory lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "sessions", session.EncodePath(sub))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("subdirectory launch created a cwd-keyed session dir (stat err %v)", err)
	}
	entries, err := session.Read(session.Path(repo, "proj-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("root-keyed transcript has %d entries, want 4 (both launches in one file)", len(entries))
	}
	if entries[3].CWD != sub {
		t.Errorf("entry cwd = %q, want the launch directory %q", entries[3].CWD, sub)
	}
}
