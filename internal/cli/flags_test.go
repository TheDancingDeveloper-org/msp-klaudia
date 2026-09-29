package cli

import (
	"path/filepath"
	"testing"
)

// TestSessionRootsAddsExtraDirs is the --add-dir contract: an extra directory
// becomes a project root (in-scope work), not a host directory the gate would
// prompt on. It exercises the same helper the host gate's Roots closure uses.
func TestSessionRootsAddsExtraDirs(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	extra := t.TempDir()

	roots := sessionRoots(home, cwd, []string{extra})

	// trust.NewRoots canonicalises, so compare against the resolved forms.
	wantCWD, _ := filepath.EvalSymlinks(cwd)
	wantExtra, _ := filepath.EvalSymlinks(extra)
	has := func(p string) bool {
		for _, r := range roots.Project {
			if r == p {
				return true
			}
		}
		return false
	}
	if !has(wantCWD) {
		t.Errorf("cwd %q missing from project roots %v", wantCWD, roots.Project)
	}
	if !has(wantExtra) {
		t.Errorf("--add-dir %q missing from project roots %v", wantExtra, roots.Project)
	}

	// With no extra dirs the working directory is still a root, unchanged.
	if base := sessionRoots(home, cwd, nil); len(base.Project) != 1 || base.Project[0] != wantCWD {
		t.Errorf("no extra dirs: project roots = %v, want just %q", base.Project, wantCWD)
	}
}

// TestNewRootCommandRegistersFlags guards the four flags stay wired (a typo in
// the flag name or a dropped registration would silently make them no-ops).
func TestNewRootCommandRegistersFlags(t *testing.T) {
	f := NewRootCommand().Flags()
	for _, name := range []string{"system-prompt", "append-system-prompt", "mcp-config", "add-dir"} {
		if f.Lookup(name) == nil {
			t.Errorf("flag --%s is not registered", name)
		}
	}
}
