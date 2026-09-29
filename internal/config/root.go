package config

import (
	"os"
	"path/filepath"
)

// Root returns Klaudia's user-level directory: $KLAUDIA_CONFIG_DIR when set,
// otherwise ~/.klaudia. It holds the global config.toml and .mcp.json, user
// skills, sessions, job logs and the browser profile, so every one of those
// must be found through here — a caller that joins its own ~/.klaudia sends
// part of Klaudia's state somewhere the override does not reach. Root returns
// "" when the variable is unset and the home directory cannot be determined.
func Root() string {
	if dir := os.Getenv("KLAUDIA_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".klaudia")
}

// GlobalPath returns the user-level config file, Root()/config.toml, or ""
// when Root cannot be determined.
func GlobalPath() string {
	root := Root()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "config.toml")
}
