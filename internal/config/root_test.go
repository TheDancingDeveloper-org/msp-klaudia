package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootDefaultsToHomeKlaudia(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")

	if got, want := Root(), filepath.Join(home, ".klaudia"); got != want {
		t.Errorf("Root() = %q, want %q", got, want)
	}
	if got, want := GlobalPath(), filepath.Join(home, ".klaudia", "config.toml"); got != want {
		t.Errorf("GlobalPath() = %q, want %q", got, want)
	}
}

func TestRootHonoursKlaudiaConfigDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", dir)

	if got := Root(); got != dir {
		t.Errorf("Root() = %q, want %q", got, dir)
	}
	if got, want := GlobalPath(), filepath.Join(dir, "config.toml"); got != want {
		t.Errorf("GlobalPath() = %q, want %q", got, want)
	}
}

// The reported bug: sessions, MCP and job logs followed KLAUDIA_CONFIG_DIR but
// config.toml itself was still read from ~/.klaudia.
func TestLoadReadsGlobalConfigFromKlaudiaConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, `model = "from-home"`)

	dir := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`model = "from-config-dir"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if cfg := mustLoad(t, t.TempDir()); cfg.Model != "from-config-dir" {
		t.Errorf("model = %q, want from-config-dir (~/.klaudia must not be read when KLAUDIA_CONFIG_DIR is set)", cfg.Model)
	}
}
