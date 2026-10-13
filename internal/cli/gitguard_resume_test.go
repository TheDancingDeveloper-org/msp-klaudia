package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/session"
)

// A session keeps its baseline beside its transcript and reads it back on a
// resume; a fork with no file of its own yet reads the parent's (WI-1300).
func TestGuardBaselinePaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	parent := session.Path(root, "parent")

	path, from := guardBaselinePaths(root, "parent", "", "")
	if path != session.GitguardPathFor(parent) || from != path {
		t.Fatalf("new session: path=%s from=%s", path, from)
	}
	if err := os.MkdirAll(filepath.Dir(parent), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{parent, session.GitguardPathFor(parent)} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if path, from = guardBaselinePaths(root, "parent", parent, "parent"); path != from || path != session.GitguardPathFor(parent) {
		t.Fatalf("resume: path=%s from=%s", path, from)
	}
	path, from = guardBaselinePaths(root, "child", "", "parent")
	if path != session.GitguardPathFor(session.Path(root, "child")) || from != session.GitguardPathFor(parent) {
		t.Fatalf("fork: path=%s from=%s", path, from)
	}
}
