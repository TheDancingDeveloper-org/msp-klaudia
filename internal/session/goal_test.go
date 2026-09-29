package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoalSidecarRoundTrip(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "proj", "abc.jsonl")
	path := GoalPathFor(transcript)
	if want := filepath.Join(filepath.Dir(transcript), "abc.goal"); path != want {
		t.Fatalf("GoalPathFor = %q, want %q", path, want)
	}
	if got := ReadGoal(path); got != "" {
		t.Fatalf("ReadGoal with no file = %q, want empty", got)
	}
	if err := WriteGoal(path, "  ship the importer \n"); err != nil {
		t.Fatal(err)
	}
	if got := ReadGoal(path); got != "ship the importer" {
		t.Fatalf("ReadGoal = %q", got)
	}
	// Clearing removes the file, so a cleared goal is not restored.
	if err := WriteGoal(path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleared goal file still present: %v", err)
	}
	// Clearing a goal that was never set is not an error.
	if err := WriteGoal(path, ""); err != nil {
		t.Fatalf("clearing an absent goal: %v", err)
	}
}
