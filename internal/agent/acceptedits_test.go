package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/sandbox"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// acceptEditsRun dispatches one call in acceptEdits through the real loop path
// (host gate, then the permission check, then the approver) and reports
// whether the approver was asked and what came back.
type acceptEditsRun struct {
	t     *testing.T
	l     *Loop
	opts  Options
	asked []string
}

func newAcceptEditsRun(t *testing.T, proj string, extra ...string) *acceptEditsRun {
	t.Helper()
	g, _ := gateFixture(t)
	write, err := tools.NewWrite()
	if err != nil {
		t.Fatal(err)
	}
	// The real Bash, not the stub: its own permission stance is what is
	// under test. Every ask is declined, so nothing it is given runs.
	bash, err := tools.NewBash(sandbox.NewLocal())
	if err != nil {
		t.Fatal(err)
	}
	r := &acceptEditsRun{t: t, l: New(nil, tools.NewRegistry(write, bash))}
	r.opts = Options{
		WorkingDir: proj,
		ExtraDirs:  extra,
		Host:       g,
		Permission: permission.Context{Mode: permission.StaticMode(permission.ModeAcceptEdits)},
		// Decline every ask, as an unattended frontend would, and record it.
		Approver: ApproverFunc(func(_ context.Context, req ApprovalRequest) permission.Decision {
			r.asked = append(r.asked, req.ToolName)
			return permission.Decision{Behavior: permission.Deny, Message: "declined"}
		}),
	}
	return r
}

func (r *acceptEditsRun) call(name string, input any) string {
	r.t.Helper()
	raw, _ := json.Marshal(input)
	tu := anthropic.BetaToolUseBlock{ID: "t", Name: name, Input: json.RawMessage(raw)}
	return resultText(r.l.dispatch(context.Background(), tu, r.opts, 0, nil, nil, newFailureState()))
}

func acceptEditsProject(t *testing.T) (proj, outside string) {
	t.Helper()
	base := t.TempDir()
	proj, outside = filepath.Join(base, "proj"), filepath.Join(base, "outside")
	for _, d := range []string{proj, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return proj, outside
}

// An edit in the working directory runs without anyone being asked.
func TestAcceptEditsWritesInTheWorkspaceWithoutAsking(t *testing.T) {
	proj, _ := acceptEditsProject(t)
	r := newAcceptEditsRun(t, proj)
	r.call("Write", map[string]string{"file_path": "notes.txt", "content": "hi"})
	if len(r.asked) != 0 {
		t.Fatalf("an edit inside the workspace was asked about: %v", r.asked)
	}
	if got, err := os.ReadFile(filepath.Join(proj, "notes.txt")); err != nil || string(got) != "hi" {
		t.Fatalf("notes.txt = %q, %v; want the write to have happened", got, err)
	}
}

// A shell command is put to the user, and declining it means it never runs.
func TestAcceptEditsAsksBeforeACommand(t *testing.T) {
	proj, _ := acceptEditsProject(t)
	r := newAcceptEditsRun(t, proj)
	body := r.call("Bash", map[string]string{"command": "touch ran.txt"})
	if len(r.asked) != 1 || r.asked[0] != "Bash" {
		t.Fatalf("asked = %v, want one ask for Bash", r.asked)
	}
	if _, err := os.Stat(filepath.Join(proj, "ran.txt")); err == nil {
		t.Fatal("a declined command ran")
	}
	if !strings.Contains(body, "declined") {
		t.Errorf("the result should carry the refusal, got %s", body)
	}
}

// An edit outside the workspace asks, even where the host gate has nothing to
// say about it (a sibling directory is not part of this machine's OS).
func TestAcceptEditsAsksBeforeAnEditOutsideTheWorkspace(t *testing.T) {
	proj, outside := acceptEditsProject(t)
	r := newAcceptEditsRun(t, proj)
	target := filepath.Join(outside, "x.txt")
	r.call("Write", map[string]string{"file_path": target, "content": "x"})
	if len(r.asked) != 1 || r.asked[0] != "Write" {
		t.Fatalf("asked = %v, want one ask for the Write", r.asked)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a declined edit outside the workspace reached the disk")
	}
	// The same path written relatively, climbing out of the project, is
	// the same edit and asks the same way.
	r.call("Write", map[string]string{"file_path": "../outside/y.txt", "content": "y"})
	if len(r.asked) != 2 {
		t.Fatalf("a relative path out of the workspace was not asked about: %v", r.asked)
	}
}

// An added directory is part of the workspace; a symlink inside the project
// that points out of it is not.
func TestAcceptEditsWorkspaceIncludesAddedDirsButNotEscapingLinks(t *testing.T) {
	proj, outside := acceptEditsProject(t)
	r := newAcceptEditsRun(t, proj, outside)
	r.call("Write", map[string]string{"file_path": filepath.Join(outside, "ok.txt"), "content": "ok"})
	if len(r.asked) != 0 {
		t.Fatalf("an edit in an added directory was asked about: %v", r.asked)
	}

	proj2, outside2 := acceptEditsProject(t)
	if err := os.Symlink(outside2, filepath.Join(proj2, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r2 := newAcceptEditsRun(t, proj2)
	r2.call("Write", map[string]string{"file_path": "link/z.txt", "content": "z"})
	if len(r2.asked) != 1 {
		t.Fatalf("an edit through a link out of the workspace was not asked about: %v", r2.asked)
	}
	if _, err := os.Stat(filepath.Join(outside2, "z.txt")); err == nil {
		t.Fatal("a declined edit through an escaping link reached the disk")
	}
}

// The model is told the mode, so it can prefer edits to shell commands rather
// than find out by being asked.
func TestAcceptEditsIsDescribedToTheModel(t *testing.T) {
	clause := modeClause(permission.ModeAcceptEdits)
	for _, want := range []string{"accept edits", "shell commands", "outside the workspace"} {
		if !strings.Contains(clause, want) {
			t.Errorf("acceptEdits clause does not mention %q:\n%s", want, clause)
		}
	}
	if modeClause(permission.ModeAutonomous) != "" {
		t.Error("autonomous should still add no clause")
	}
}
