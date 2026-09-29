package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// projectRoot is the directory project-scoped state is keyed by: memory
// (.klaudia/MEMORY.md, KNOWLEDGE.md), session transcripts and project skills.
// It is the git top-level of cwd, so starting Klaudia from a subdirectory of a
// repository finds the same memory, auto-resumes the same session and loads
// the same skills as starting it at the top — as CLAUDE.md lookup already did.
// Outside a repository it is cwd. Tools still run in cwd; only where that
// state lives changes.
//
// The root is spelled through cwd's own path where it can be: git reports the
// top-level with symlinks resolved, and keying sessions by that would move an
// existing repo-root session dir whenever the checkout is reached through a
// symlink. So cwd joined with --show-cdup is preferred, and the resolved
// top-level is used only when that does not name the same directory.
//
// A top-level that is the home directory is ignored: a dotfiles repository in
// $HOME would otherwise make every non-repository directory below it one
// project, sharing a single memory and session list (in ~/.klaudia, which is
// also the user config dir).
func projectRoot(cwd string) string {
	if cwd == "" {
		return cwd
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel", "--show-cdup")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return cwd
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	top := strings.TrimSpace(lines[0])
	if top == "" {
		return cwd
	}
	root := top
	if len(lines) > 1 {
		if cand := filepath.Clean(filepath.Join(cwd, strings.TrimSpace(lines[1]))); sameDir(cand, top) {
			root = cand
		}
	} else if sameDir(cwd, top) {
		root = cwd // at the top-level, --show-cdup prints an empty line
	}
	if home, err := os.UserHomeDir(); err == nil && sameDir(root, home) {
		return cwd
	}
	return root
}

// sameDir reports whether a and b name the same existing directory.
func sameDir(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}
