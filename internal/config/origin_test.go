package config

import (
	"strings"
	"testing"
)

// find returns the setting for key, or a zero Setting with ok=false.
func find(settings []Setting, key string) (Setting, bool) {
	for _, s := range settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

func TestOriginsLayeringAndPrecedence(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)

	// Home sets provider, model, baseURL and sandbox.mode.
	writeConfig(t, home, `
provider = "openai"
model = "home-model"
baseURL = "https://home.example/v1"
[sandbox]
mode = "os"
`)
	// Project overrides model and adds theme; leaves provider/baseURL to home.
	writeConfig(t, cwd, `
model = "project-model"
theme = "nord"
`)

	settings := Origins(cwd)

	// A value set only in home reports origin=home.
	if s, ok := find(settings, "baseURL"); !ok || s.Origin != OriginHome || s.Value != "https://home.example/v1" {
		t.Errorf("baseURL = %+v ok=%v, want home", s, ok)
	}
	// A value set in both reports origin=project (project overrides home).
	if s, ok := find(settings, "model"); !ok || s.Origin != OriginProject || s.Value != "project-model" {
		t.Errorf("model = %+v ok=%v, want project override", s, ok)
	}
	// A value set only in project reports origin=project.
	if s, ok := find(settings, "theme"); !ok || s.Origin != OriginProject {
		t.Errorf("theme = %+v ok=%v, want project", s, ok)
	}
	// provider set only in home.
	if s, ok := find(settings, "provider"); !ok || s.Origin != OriginHome || s.Value != "openai" {
		t.Errorf("provider = %+v ok=%v, want home openai", s, ok)
	}
	if s, ok := find(settings, "sandbox.mode"); !ok || s.Origin != OriginHome || s.Value != "os" {
		t.Errorf("sandbox.mode = %+v ok=%v, want home os", s, ok)
	}
}

func TestOriginsDefaults(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	// No config files at all: provider and sandbox.mode still report their
	// built-in defaults with origin=default.
	settings := Origins(cwd)
	if s, ok := find(settings, "provider"); !ok || s.Origin != OriginDefault || s.Value != ProviderAnthropic {
		t.Errorf("provider default = %+v ok=%v, want default anthropic", s, ok)
	}
	if s, ok := find(settings, "sandbox.mode"); !ok || s.Origin != OriginDefault || s.Value != SandboxLocal {
		t.Errorf("sandbox.mode default = %+v ok=%v, want default local", s, ok)
	}
	// A field with no default and no file value is omitted entirely.
	if s, ok := find(settings, "model"); ok {
		t.Errorf("model should be absent when unset everywhere, got %+v", s)
	}
}

func TestOriginsAPIKeyRedactionInline(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	// An inline apiKey in a file must never be printed: the value is a
	// redaction note, and the origin is the file that set it.
	const secret = "sk-do-not-print-me"
	writeConfig(t, cwd, "apiKey = \""+secret+"\"\n")

	settings := Origins(cwd)
	s, ok := find(settings, "apiKey")
	if !ok {
		t.Fatal("apiKey setting absent")
	}
	if s.Value == secret || strings.Contains(s.Value, secret) {
		t.Fatalf("apiKey value leaked the secret: %q", s.Value)
	}
	if s.Origin != OriginProject {
		t.Errorf("inline apiKey origin = %q, want project", s.Origin)
	}
	// Sanity: the secret must not appear in ANY rendered value.
	for _, st := range settings {
		if strings.Contains(st.Value, secret) {
			t.Fatalf("secret leaked in setting %q = %q", st.Key, st.Value)
		}
	}
}

func TestOriginsAPIKeyFromEnv(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	// apiKeyEnv names a variable; when that variable is set, the key resolves
	// from the environment → origin=env, value redacted.
	writeConfig(t, cwd, `apiKeyEnv = "KLAUDIA_TEST_KEY"`+"\n")
	t.Setenv("KLAUDIA_TEST_KEY", "sk-env-secret")

	settings := Origins(cwd)
	s, ok := find(settings, "apiKey")
	if !ok {
		t.Fatal("apiKey setting absent")
	}
	if s.Origin != OriginEnv {
		t.Errorf("apiKey origin = %q, want env", s.Origin)
	}
	if strings.Contains(s.Value, "sk-env-secret") {
		t.Fatalf("apiKey value leaked the env secret: %q", s.Value)
	}
	// apiKeyEnv itself (the variable NAME, not a secret) is shown from the file.
	if e, ok := find(settings, "apiKeyEnv"); !ok || e.Value != "KLAUDIA_TEST_KEY" || e.Origin != OriginProject {
		t.Errorf("apiKeyEnv = %+v ok=%v", e, ok)
	}
}

func TestOriginsListAccumulates(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, "[lsp]\ndisabled = [\"gopls\"]\n")
	writeConfig(t, cwd, "[lsp]\ndisabled = [\"pyright\"]\n")

	settings := Origins(cwd)
	s, ok := find(settings, "lsp.disabled")
	if !ok {
		t.Fatal("lsp.disabled absent")
	}
	// Lists accumulate (home + project), and the origin reflects that project
	// contributed.
	if !strings.Contains(s.Value, "gopls") || !strings.Contains(s.Value, "pyright") {
		t.Errorf("lsp.disabled = %q, want both home and project entries", s.Value)
	}
	if s.Origin != OriginProject {
		t.Errorf("lsp.disabled origin = %q, want project (contributed)", s.Origin)
	}
}
