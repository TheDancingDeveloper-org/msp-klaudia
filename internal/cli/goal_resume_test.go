package cli

import (
	"testing"

	"github.com/greenthread-ai/klaudia/internal/session"
)

func TestStandingGoalRestoredOnResume(t *testing.T) {
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := "/work/a"
	seedSession(t, cwd, "old")

	// A fresh session starts with no goal and records it beside its transcript.
	path, g := standingGoal(cwd, "fresh", "", "")
	if g != "" || path != session.GoalPathFor(session.Path(cwd, "fresh")) {
		t.Fatalf("fresh: (%q, %q)", path, g)
	}

	oldPath, _ := standingGoal(cwd, "old", session.Path(cwd, "old"), "")
	if err := session.WriteGoal(oldPath, "finish the migration"); err != nil {
		t.Fatal(err)
	}

	// A plain resume restores it from the same file.
	path, g = standingGoal(cwd, "old", session.Path(cwd, "old"), "old")
	if g != "finish the migration" || path != oldPath {
		t.Fatalf("resume: (%q, %q)", path, g)
	}

	// A fork inherits it and records it under its own id, so resuming the
	// fork restores it as well.
	forkPath, g := standingGoal(cwd, "fork", "", "old")
	if g != "finish the migration" {
		t.Fatalf("fork inherited %q", g)
	}
	if got := session.ReadGoal(forkPath); got != "finish the migration" {
		t.Fatalf("fork's own goal file = %q", got)
	}
}
