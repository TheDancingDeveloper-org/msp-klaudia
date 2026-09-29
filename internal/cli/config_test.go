package cli

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/config"
)

func TestCreateConfig(t *testing.T) {
	tests := []struct {
		name      string
		scope     string
		wantUnder func(home, cwd string) string
	}{
		{
			name:  "global",
			scope: "global",
			wantUnder: func(home, cwd string) string {
				return filepath.Join(home, ".klaudia", "config.toml")
			},
		},
		{
			name:  "local",
			scope: "local",
			wantUnder: func(home, cwd string) string {
				return filepath.Join(cwd, ".klaudia", "config.toml")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			cwd := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("KLAUDIA_CONFIG_DIR", "")

			path, err := createConfig(tt.scope, cwd)
			if err != nil {
				t.Fatalf("createConfig() error = %v", err)
			}
			want := tt.wantUnder(home, cwd)
			if path != want {
				t.Fatalf("path = %q, want %q", path, want)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body := string(data)
			for _, want := range []string{
				"\nprovider = \"anthropic\"\n", // active, uncommented
				`#   provider = "openai"`,      // the alternative stays commented out
				`#   baseURL = "https://api.example.com/v1"`,
				`#   apiKeyEnv = "MY_API_KEY"`,
				`bypassPermissions | dontAsk`,
				`# Klaudia config`,
				`# contextWindow = 8192`, // commented example for OpenAI-compatible hosts
			} {
				if !strings.Contains(body, want) {
					t.Errorf("starter config missing %s:\n%s", want, body)
				}
			}

			if _, err := createConfig(tt.scope, cwd); err == nil || !strings.Contains(err.Error(), "config already exists") {
				t.Fatalf("second createConfig() error = %v, want already exists", err)
			}
		})
	}
}

func TestCreateConfigRejectsInvalidScope(t *testing.T) {
	if _, err := createConfig("project", t.TempDir()); err == nil || !strings.Contains(err.Error(), "global or local") {
		t.Fatalf("createConfig invalid scope error = %v", err)
	}
}

func TestThemeOrWarn(t *testing.T) {
	var warned []string
	warn := func(m string) { warned = append(warned, m) }

	// Empty → passes through, no warning.
	if got := themeOrWarn("", warn); got != "" || len(warned) != 0 {
		t.Errorf("empty theme: got %q, warnings %v", got, warned)
	}
	// Known theme → passes through, no warning.
	if got := themeOrWarn("nord", warn); got != "nord" || len(warned) != 0 {
		t.Errorf("known theme: got %q, warnings %v", got, warned)
	}
	// Unknown → falls back to default ("") and warns.
	if got := themeOrWarn("bogus", warn); got != "" {
		t.Errorf("unknown theme: got %q, want fallback to default", got)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "unknown theme") {
		t.Errorf("expected an unknown-theme warning, got %v", warned)
	}
}

// Every example in the starter config, uncommented, is a key the schema
// knows. Load warns on unknown keys, so a drifted example would otherwise
// greet a new user with a warning about the file we wrote for them.
func TestStarterConfigExamplesAreKnownKeys(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	path, err := createConfig("local", cwd)
	if err != nil {
		t.Fatal(err)
	}
	example := regexp.MustCompile(`^# ([A-Za-z]+ = |\[[A-Za-z]+\]$)`)
	var lines []string
	uncommented := 0
	for _, line := range strings.Split(starterConfig, "\n") {
		if example.MatchString(line) {
			line = strings.TrimPrefix(line, "# ")
			uncommented++
		}
		lines = append(lines, line)
	}
	if uncommented < 5 {
		t.Fatalf("uncommented only %d example lines; the pattern has drifted from the starter", uncommented)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		t.Fatalf("starter with examples uncommented does not load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("starter examples use unknown keys: %v", cfg.Warnings)
	}
}

func TestStarterConfigRoundtripsContextWindow(t *testing.T) {
	// Generate the starter, uncomment the contextWindow example, parse it back
	// through config.Load and confirm the field actually populates. Catches the
	// case where the toml tag drifts from the example line.
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	path, err := createConfig("local", cwd)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := strings.Replace(string(data), "# contextWindow = 8192", "contextWindow = 8192", 1)
	if got == string(data) {
		t.Fatal("commented contextWindow line not found in starter — toml tag/example may have drifted")
	}
	if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextWindow != 8192 {
		t.Errorf("ContextWindow = %d, want 8192 (toml tag mismatch?)", cfg.ContextWindow)
	}
}

func TestWithNoticesWritesNoticesToStderr(t *testing.T) {
	var stderr strings.Builder
	var passed []string
	emit := withNotices(func(ev agent.Event) { passed = append(passed, ev.Type) }, &stderr)
	emit(agent.Event{Type: "assistant", Text: "hello"})
	emit(agent.Event{Type: "notice", Content: "Model x is overloaded; retrying this request on fallback model y."})

	if got := stderr.String(); got != "note: Model x is overloaded; retrying this request on fallback model y.\n" {
		t.Errorf("stderr = %q", got)
	}
	// The wrapped emitter still sees every event, so stream-json output is
	// unchanged.
	if len(passed) != 2 {
		t.Errorf("wrapped emitter saw %v, want both events", passed)
	}
}

// runWithConfig runs one headless stream-json launch in a folder whose
// .klaudia/config.toml is body, and returns what went to stderr and the error.
func runWithConfig(t *testing.T, body string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".klaudia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ProjectPath(cwd), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	in := strings.NewReader(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n")
	var stderr strings.Builder
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-mode", "dontAsk"})
	cmd.SetIn(in)
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(context.Background())
	return stderr.String(), err
}

func TestBrokenConfigIsAUsageError(t *testing.T) {
	_, err := runWithConfig(t, "provider = \"openai\"\n[sandbox\n")
	if !isUsageError(err) {
		t.Fatalf("err = %v (exit %d), want a usage error (exit %d)", err, exitCodeFor(err), ExitUsage)
	}
	if !strings.Contains(err.Error(), "config.toml:2:") {
		t.Errorf("error %q does not name the file and line", err)
	}
}

func TestUnknownConfigKeyWarnsAndRuns(t *testing.T) {
	srv := httptest.NewServer(&fakeChat{})
	defer srv.Close()
	stderr, err := runWithConfig(t, fmt.Sprintf(
		"provider = \"openai\"\nbaseURL = %q\napiKey = \"test\"\nmodel = \"fake-model\"\nmodle = \"typo\"\n", srv.URL))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !strings.Contains(stderr, `config.toml:5: unknown key "modle", ignored`) {
		t.Errorf("stderr %q does not warn about the unknown key", stderr)
	}
}

// --create-config=global and /doctor's config check must use the same file
// config.Load reads: $KLAUDIA_CONFIG_DIR/config.toml when the variable is set.
func TestCreateConfigGlobalHonoursKlaudiaConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(t.TempDir(), "klaudia-config")
	t.Setenv("KLAUDIA_CONFIG_DIR", dir)
	cwd := t.TempDir()

	if configFileExists(cwd) {
		t.Fatal("configFileExists = true before any config was written")
	}
	path, err := createConfig("global", cwd)
	if err != nil {
		t.Fatalf("createConfig() error = %v", err)
	}
	if want := filepath.Join(dir, "config.toml"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".klaudia", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("~/.klaudia/config.toml was written despite KLAUDIA_CONFIG_DIR (stat err = %v)", err)
	}
	if !configFileExists(cwd) {
		t.Error("configFileExists = false after writing $KLAUDIA_CONFIG_DIR/config.toml")
	}
	cfg, lerr := config.Load(cwd)
	if lerr != nil {
		t.Fatalf("config.Load: %v", lerr)
	}
	if cfg.Provider != "openai" {
		t.Errorf("config.Load provider = %q, want openai from the starter it just wrote", cfg.Provider)
	}
}
