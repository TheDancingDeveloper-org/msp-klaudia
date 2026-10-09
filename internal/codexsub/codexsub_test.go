package codexsub

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeCodex is a stand-in for the codex binary. It records the arguments it
// was invoked with and emits a scripted --json event stream, which is all the
// runner reads back.
func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	// The runner probes `codex exec --help` before every spawn. A script that
	// does not special-case it would run the task twice and report the probe's
	// empty output as a missing --session-id, which is what the real binary
	// does today.
	wrapped := "if [ \"$1\" = \"exec\" ] && [ \"$2\" = \"--help\" ]; then echo \"$CODEXSUB_HELP\"; exit 0; fi\n" + body
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+wrapped), 0o755); err != nil {
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
	t.Chdir(root)
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
`), Roots: []string{root}}

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

func TestChildArgv(t *testing.T) {
	root := repo(t)
	args := filepath.Join(t.TempDir(), "args")
	env := filepath.Join(t.TempDir(), "env")
	bin := fakeCodex(t, `printf '%s\n' "$*" > "$CODEXSUB_ARGS"
env > "$CODEXSUB_ENV"
`+echoAgent("done"))
	t.Setenv("CODEXSUB_ARGS", args)
	t.Setenv("CODEXSUB_ENV", env)
	r := &Runner{Bin: bin}

	if _, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	// The posture is set on every spawn, not inherited. A sandboxed parent
	// must not be able to get an unsandboxed child out of this.
	for _, want := range []string{"-s workspace-write", "--cd", "--json", "--ephemeral"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("args missing %q: %s", want, got)
		}
	}
	if strings.Contains(string(got), "-a ") {
		t.Errorf("codex exec has no approval flag; passing one fails every spawn: %s", got)
	}
	if strings.Contains(string(got), "--session-id") {
		t.Errorf("passed --session-id before WI-1126: %s", got)
	}
	e, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e), "KLAUDIA_CODEXSUB_DEPTH=1") {
		t.Errorf("child env = %s", e)
	}
	if strings.Contains(string(e), "CODEX_NONINTERACTIVE") {
		t.Errorf("CODEX_NONINTERACTIVE is not a Codex variable: %s", e)
	}
}

func TestSessionIDIsUUID(t *testing.T) {
	root := repo(t)
	args := filepath.Join(t.TempDir(), "args")
	bin := fakeCodex(t, `printf '%s\n' "$*" > "$CODEXSUB_ARGS"
`+echoAgent("done"))
	t.Setenv("CODEXSUB_ARGS", args)
	t.Setenv("CODEXSUB_HELP", "--session-id <uuid>")
	r := &Runner{Bin: bin}

	res, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(args)
	if !strings.Contains(string(got), "--session-id "+res.SessionID) {
		t.Fatalf("args %s, session %q", got, res.SessionID)
	}
	if _, err := uuid.Parse(res.SessionID); err != nil {
		t.Fatalf("session id %q is not a UUID", res.SessionID)
	}
	if strings.Contains(res.SessionID, filepath.Base(root)) {
		t.Fatal("session id is the directory name, which WI-1126 will reject")
	}
}

// TestRealCodexAcceptsOurFlags checks the argv against an actual codex, which
// the fake script cannot: a fake accepts whatever it is handed, and -a never
// was shipped that way even though `codex exec` rejects it.
//
// Set KLAUDIA_TEST_CODEX to the binary, or leave it unset to use codex on
// PATH. The wrapper on a Vogt pod is skipped: it injects
// --dangerously-bypass-approvals-and-sandbox, which conflicts with -s, so its
// help says nothing about the binary underneath.
func TestRealCodexAcceptsOurFlags(t *testing.T) {
	bin := os.Getenv("KLAUDIA_TEST_CODEX")
	if bin == "" {
		var err error
		bin, err = exec.LookPath("codex")
		if err != nil {
			t.Skip("no codex on PATH")
		}
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	// A shell wrapper is what injects the bypass flag. The native binary mentions
	// the same flag in its own help text, and skipping it would skip the
	// check this test exists to make.
	head := b
	if len(head) > 256 {
		head = head[:256]
	}
	if bytes.Contains(head, []byte("#!/")) && bytes.Contains(b, []byte("dangerously-bypass-approvals-and-sandbox")) {
		t.Skip("codex on PATH is the full-access wrapper, not the binary")
	}
	out, _ := exec.Command(bin, "exec", "--help").CombinedOutput()
	help := string(out)
	for _, flag := range []string{"--cd", "-s, --sandbox", "--json", "--ephemeral", "--output-schema", "-o, --output-last-message"} {
		if !strings.Contains(help, flag) {
			t.Errorf("codex exec --help does not accept %s", flag)
		}
	}
	if strings.Contains(help, "--session-id") {
		t.Log("--session-id is accepted; sessionIDSupported should now be true")
	}
}

func TestWorkingDirConfined(t *testing.T) {
	root := repo(t)
	r := &Runner{Bin: fakeCodex(t, echoAgent("done")), Roots: []string{root}}

	outside := t.TempDir()
	if _, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: outside, Isolation: "shared"}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside path: %v", err)
	}

	// A symlink inside the root that points outside it is the same escape.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: link, Isolation: "shared"}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("symlink escape: %v", err)
	}
}

func TestDepthAndJobCaps(t *testing.T) {
	root := repo(t)
	r := &Runner{Bin: fakeCodex(t, echoAgent("done")), Depth: 1}
	if _, err := r.Run(context.Background(), Request{Task: "do it", WorkingDir: root}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("nested spawn: %v", err)
	}

	held := make(chan struct{})
	slow := fakeCodex(t, "while [ ! -f \"$CODEXSUB_RELEASE\" ]; do sleep 0.01; done\n"+echoAgent("done"))
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("CODEXSUB_RELEASE", release)
	capped := &Runner{Bin: slow, MaxJobs: 1}
	if _, err := capped.Run(context.Background(), Request{Task: "do it", WorkingDir: root, Background: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := capped.Run(context.Background(), Request{Task: "do it", WorkingDir: root, Background: true}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over cap: %v", err)
	}
	close(held)
	if err := os.WriteFile(release, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
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
