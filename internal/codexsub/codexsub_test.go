package codexsub

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCodex is a stand-in for the codex binary. It records the arguments it
// was invoked with and emits a scripted --json event stream, which is all the
// runner reads back.
func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// echoAgent emits one agent message and writes it to the -o file, the shape a
// successful `codex exec --json` leaves behind.
func echoAgent(text string) string {
	return `cd=""
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --cd) cd="$2"; shift 2;;
    -o) out="$2"; shift 2;;
    *) shift;;
  esac
done
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":` + text + `}}'
if [ -n "$out" ]; then printf '%s' '` + text + `' > "$out"; fi
if [ -n "$cd" ]; then printf 'child was here\n' > "$cd/child.txt"; fi
`
}

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	sh := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	sh("init", "--quiet")
	sh("config", "user.name", "Test")
	sh("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh("add", "-A")
	sh("commit", "--quiet", "-m", "first")
	return root
}

func TestSpawnAdoptsChildChange(t *testing.T) {
	root := repo(t)
	// An uncommitted edit must reach the child: the worktree is seeded from the
	// working tree, not from HEAD.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: fakeCodex(t, `cd=""
while [ $# -gt 0 ]; do
  case "$1" in
    --cd) cd="$2"; shift 2;;
    *) shift;;
  esac
done
if ! grep -q dirty "$cd/a.txt"; then echo "seed missing the parent's edit" >&2; exit 1; fi
printf 'child was here\n' > "$cd/child.txt"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
`)}

	res, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "done" {
		t.Fatalf("final = %q", res.Final)
	}
	if len(res.Adopted) != 1 || res.Adopted[0] != "child.txt" {
		t.Fatalf("adopted = %v", res.Adopted)
	}
	if res.Conflicted != nil {
		t.Fatalf("conflicted = %v", res.Conflicted)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "child.txt")); string(got) != "child was here\n" {
		t.Fatalf("parent tree = %q", got)
	}
}

func TestSchemaMismatchAdoptsNothing(t *testing.T) {
	root := repo(t)
	r := &Runner{Bin: fakeCodex(t, echoAgent("not json"))}

	_, err := r.Run(context.Background(), Request{
		Task:         "do it",
		WorkingDir:   root,
		OutputSchema: json.RawMessage(`{"type":"object","required":["ok"]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("got %v, want a schema error", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "child.txt")); !os.IsNotExist(statErr) {
		t.Fatal("a schema mismatch must not change the parent's tree")
	}
}

func TestSchemaMatch(t *testing.T) {
	root := repo(t)
	r := &Runner{Bin: fakeCodex(t, echoAgent(`{"ok":true}`))}

	res, err := r.Run(context.Background(), Request{
		Task:         "do it",
		WorkingDir:   root,
		OutputSchema: json.RawMessage(`{"type":"object","required":["ok"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != `{"ok":true}` {
		t.Fatalf("final = %q", res.Final)
	}
}

func TestChildFailure(t *testing.T) {
	root := repo(t)
	r := &Runner{Bin: fakeCodex(t, "echo 'codex blew up' >&2; exit 3\n")}

	_, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root})
	if err == nil || !strings.Contains(err.Error(), "codex blew up") {
		t.Fatalf("got %v, want the child's stderr", err)
	}
}

func TestAdoptConflict(t *testing.T) {
	root := repo(t)
	// The parent deletes the tracked file after the worktree is seeded but
	// before the child edits it. The child waits on a marker written only once
	// the file is gone, so the two cannot race. The child's patch then names a
	// file that no longer exists, which is the case Adopt reports as a conflict
	// rather than overwriting.
	marker := filepath.Join(t.TempDir(), "parent-moved")
	started := make(chan struct{})
	r := &Runner{Bin: fakeCodex(t, `cd=""
while [ $# -gt 0 ]; do
  case "$1" in
    --cd) cd="$2"; shift 2;;
    *) shift;;
  esac
done
printf 'x' > "$CODEXSUB_STARTED"
i=0
while [ ! -f "$CODEXSUB_MARKER" ]; do i=$((i+1)); [ $i -gt 500 ] && exit 9; sleep 0.01; done
printf 'child line\n' >> "$cd/a.txt"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
`)}
	t.Setenv("CODEXSUB_MARKER", marker)
	t.Setenv("CODEXSUB_STARTED", filepath.Join(t.TempDir(), "started"))

	go func() {
		for i := 0; i < 500 && !exists(os.Getenv("CODEXSUB_STARTED")); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		close(started)
		_ = os.Remove(filepath.Join(root, "a.txt"))
		_ = os.WriteFile(marker, []byte("x"), 0o644)
	}()
	res, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicted) != 1 || res.Conflicted[0] != "a.txt" {
		t.Fatalf("conflicted = %v, adopted = %v", res.Conflicted, res.Adopted)
	}
	if _, statErr := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(statErr) {
		t.Fatal("the conflicted file was written anyway")
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestBackgroundPolling(t *testing.T) {
	root := repo(t)
	r := &Runner{Bin: fakeCodex(t, echoAgent("done")+"sleep 0.3\n"), Now: func() time.Time {
		return time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	}}

	res, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != stateRunning || !strings.HasPrefix(res.Job, "codexsub-") {
		t.Fatalf("start = %+v", res)
	}
	polled, err := r.Run(context.Background(), Request{Job: res.Job})
	if err != nil {
		t.Fatal(err)
	}
	if polled.State != stateRunning {
		t.Fatalf("polled too early: %+v", polled)
	}
	done, err := r.Run(context.Background(), Request{Job: res.Job, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if done.State != stateDone || done.Final != "done" {
		t.Fatalf("done = %+v", done)
	}
	if _, statErr := os.Stat(filepath.Join(root, "child.txt")); statErr != nil {
		t.Fatal("background job did not adopt back")
	}
}

func TestSharedSkipsWorktree(t *testing.T) {
	root := repo(t)
	r := &Runner{Bin: fakeCodex(t, echoAgent("done"))}

	res, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root, Isolation: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted != nil || res.Summary != "" {
		t.Fatalf("shared run reported an adoption: %+v", res)
	}
	if _, statErr := os.Stat(filepath.Join(root, "child.txt")); statErr != nil {
		t.Fatal("shared run should write straight into working_dir")
	}
}

func TestSessionIDFlagWaitsForTheFork(t *testing.T) {
	root := repo(t)
	var got string
	bin := fakeCodex(t, `printf '%s\n' "$*" > "$CODEXSUB_ARGS"
`+echoAgent("done"))
	t.Setenv("CODEXSUB_ARGS", filepath.Join(t.TempDir(), "args"))
	r := &Runner{Bin: bin}

	if _, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(os.Getenv("CODEXSUB_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	got = string(b)
	if strings.Contains(got, "--session-id") {
		t.Fatalf("passed --session-id before WI-1126: %s", got)
	}
	for _, want := range []string{"--cd", "--json", "--ephemeral", "--output-schema"} {
		if want == "--output-schema" {
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("args missing %s: %s", want, got)
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), Request{WorkingDir: t.TempDir()}); err == nil {
		t.Fatal("empty task accepted")
	}
	if _, err := r.Run(context.Background(), Request{Task: "x", WorkingDir: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("missing working_dir accepted")
	}
	if _, err := r.Run(context.Background(), Request{Task: "x", WorkingDir: t.TempDir(), Isolation: "yolo"}); err == nil {
		t.Fatal("bad isolation accepted")
	}
}
