package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The standing goal (/goal <text>) is session state that is not a message, so
// the transcript cannot carry it: it lives in a sidecar beside the transcript,
// the way the compaction summary does. The id cannot contain a dot (ValidID),
// so "<id>.goal" never names another session's transcript.

// GoalPathFor returns the standing-goal file that sits beside a transcript.
func GoalPathFor(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, ".jsonl") + ".goal"
}

// WriteGoal records the standing goal at path. An empty goal removes the file,
// so a cleared goal stays cleared across a resume.
func WriteGoal(path, goal string) error {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(goal+"\n"), 0o644)
}

// ReadGoal returns the standing goal recorded at path, or "" when there is
// none.
func ReadGoal(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
