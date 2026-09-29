package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A project's .klaudia/config.toml arrives with the checkout. Before the trust
// list, every key in it overlaid the user's own config, so cloning a repository
// and starting Klaudia in it let that repository pick the permission mode
// (bypassPermissions included), turn the host gate off, widen the sandbox,
// choose which binary runs as Chrome, and point the OpenAI-compatible provider
// at its own server with apiKeyEnv naming any variable in the user's
// environment — the key, or anything else, sent with the first request.
//
// Keys like those now apply only in a folder the user has trusted. The list
// lives in its own file rather than in ~/.klaudia/config.toml because Klaudia
// appends to it (--trust-project, --create-config=local), and re-marshalling
// the user's config would drop the comments they wrote there.

// TrustedProjectsPath returns ~/.klaudia/trusted-projects: one absolute
// directory per line; blank lines and lines starting with # are ignored.
func TrustedProjectsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".klaudia", "trusted-projects"), nil
}

// canonicalDir resolves dir to an absolute, symlink-free path, so a trusted
// entry and the directory Klaudia starts in compare equal however each was
// reached. A path that cannot be resolved is compared as cleaned.
func canonicalDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// trustedProjects reads the trust list. A missing file is an empty list.
func trustedProjects() ([]string, error) {
	path, err := TrustedProjectsPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var dirs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		dirs = append(dirs, canonicalDir(line))
	}
	return dirs, sc.Err()
}

// IsTrustedProject reports whether cwd is on the trust list. Only the exact
// directory counts: trusting a parent does not trust the checkouts beneath it,
// since those are the repositories whose config is in question.
func IsTrustedProject(cwd string) bool {
	dirs, err := trustedProjects()
	if err != nil {
		return false
	}
	want := canonicalDir(cwd)
	for _, d := range dirs {
		if d == want {
			return true
		}
	}
	return false
}

// TrustProject adds cwd to the trust list. It reports false when cwd was
// already trusted.
func TrustProject(cwd string) (bool, error) {
	if IsTrustedProject(cwd) {
		return false, nil
	}
	path, err := TrustedProjectsPath()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := fmt.Fprintln(f, canonicalDir(cwd)); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// withholdUntrusted clears, in a project config from an untrusted folder, every
// key that can widen what Klaudia does or where it sends credentials, and
// returns the names of those that were set. What is left is preference and
// tuning (model, theme, contextWindow, maxTokens, temperature, input, lsp,
// browser display settings) and anything that can only narrow: deny rules and
// a read-only sandbox mount.
func withholdUntrusted(c *Config) []string {
	var held []string
	hold := func(set bool, name string, clear func()) {
		if set {
			held = append(held, name)
			clear()
		}
	}
	hold(c.Provider != "", "provider", func() { c.Provider = "" })
	hold(c.BaseURL != "", "baseURL", func() { c.BaseURL = "" })
	hold(c.APIKey != "", "apiKey", func() { c.APIKey = "" })
	hold(c.APIKeyEnv != "", "apiKeyEnv", func() { c.APIKeyEnv = "" })
	hold(len(c.ExtraHeadersEnv) > 0, "extraHeadersEnv", func() { c.ExtraHeadersEnv = nil })
	hold(c.Permissions.Mode != "", "permissions.mode", func() { c.Permissions.Mode = "" })
	hold(len(c.Permissions.Allow) > 0, "permissions.allow", func() { c.Permissions.Allow = nil })
	hold(c.Trust.Mode != "", "trust.mode", func() { c.Trust.Mode = "" })
	hold(c.Sandbox.Mode != "", "sandbox.mode", func() { c.Sandbox.Mode = "" })
	hold(len(c.Sandbox.WriteRoots) > 0, "sandbox.writeRoots", func() { c.Sandbox.WriteRoots = nil })
	hold(c.Sandbox.Runtime != "", "sandbox.runtime", func() { c.Sandbox.Runtime = "" })
	hold(c.Sandbox.Image != "", "sandbox.image", func() { c.Sandbox.Image = "" })
	hold(c.Sandbox.MountCWD != nil, "sandbox.mountCwd", func() { c.Sandbox.MountCWD = nil })
	hold(c.Sandbox.Network != "", "sandbox.network", func() { c.Sandbox.Network = "" })
	hold(c.Browser.ChromePath != "", "browser.chromePath", func() { c.Browser.ChromePath = "" })
	hold(c.Browser.RemoteURL != "", "browser.remoteUrl", func() { c.Browser.RemoteURL = "" })
	hold(c.Browser.UserDataDir != "", "browser.userDataDir", func() { c.Browser.UserDataDir = "" })
	return held
}
