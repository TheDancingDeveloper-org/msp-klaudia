package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	kd := filepath.Join(dir, ".klaudia")
	if err := os.MkdirAll(kd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kd, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trust puts dir on the trust list, so its project config applies in full.
func trust(t *testing.T, dir string) {
	t.Helper()
	if _, err := TrustProject(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProjectOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	writeConfig(t, home, `provider = "anthropic"
model = "sonnet"
`)

	cwd := t.TempDir()
	writeConfig(t, cwd, `provider = "openai"
baseURL = "https://x/v1"
`)
	trust(t, cwd)

	cfg := mustLoad(t, cwd)
	if cfg.Provider != "openai" {
		t.Errorf("provider = %q, want openai (project wins)", cfg.Provider)
	}
	if cfg.BaseURL != "https://x/v1" {
		t.Errorf("baseURL = %q", cfg.BaseURL)
	}
	// model not set in project → inherited from home.
	if cfg.Model != "sonnet" {
		t.Errorf("model = %q, want sonnet (inherited)", cfg.Model)
	}
}

func TestLoadFallbackModelProjectOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, `fallbackModel = "haiku"`)

	if cfg := mustLoad(t, t.TempDir()); cfg.FallbackModel != "haiku" {
		t.Errorf("fallbackModel = %q, want haiku (from home)", cfg.FallbackModel)
	}
	cwd := t.TempDir()
	writeConfig(t, cwd, `fallbackModel = "sonnet"`)
	if cfg := mustLoad(t, cwd); cfg.FallbackModel != "sonnet" {
		t.Errorf("fallbackModel = %q, want sonnet (project wins)", cfg.FallbackModel)
	}
}

func TestLoadThemeProjectOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	writeConfig(t, home, `theme = "nord"`)

	// Global-only: inherited.
	if cfg := mustLoad(t, t.TempDir()); cfg.Theme != "nord" {
		t.Errorf("theme = %q, want nord (from home)", cfg.Theme)
	}

	// Project overrides global.
	cwd := t.TempDir()
	writeConfig(t, cwd, `theme = "dracula"`)
	trust(t, cwd)
	if cfg := mustLoad(t, cwd); cfg.Theme != "dracula" {
		t.Errorf("theme = %q, want dracula (project wins)", cfg.Theme)
	}
}

func TestLoadTUINotifyOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, "[tui]\nnotify = \"bell\"\n")

	// Global-only: inherited.
	if cfg := mustLoad(t, t.TempDir()); cfg.TUI.Notify != "bell" {
		t.Errorf("tui.notify = %q, want bell (from home)", cfg.TUI.Notify)
	}

	// Project overrides global.
	cwd := t.TempDir()
	writeConfig(t, cwd, "[tui]\nnotify = \"off\"\n")
	if cfg := mustLoad(t, cwd); cfg.TUI.Notify != "off" {
		t.Errorf("tui.notify = %q, want off (project wins)", cfg.TUI.Notify)
	}
}

func TestLoadPermissionModeOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	writeConfig(t, home, "[permissions]\nmode = \"acceptEdits\"\n")

	// Global-only → inherited.
	if cfg := mustLoad(t, t.TempDir()); cfg.Permissions.Mode != "acceptEdits" {
		t.Errorf("mode = %q, want acceptEdits (from home)", cfg.Permissions.Mode)
	}
	// Project overrides global.
	cwd := t.TempDir()
	writeConfig(t, cwd, "[permissions]\nmode = \"plan\"\n")
	trust(t, cwd)
	if cfg := mustLoad(t, cwd); cfg.Permissions.Mode != "plan" {
		t.Errorf("mode = %q, want plan (project wins)", cfg.Permissions.Mode)
	}
}

func TestLoadBrowserProjectOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	writeConfig(t, home, `
[browser]
engine = "chrome"
headless = false
chromePath = "/home/chrome"
remoteUrl = "http://home:9222"
userDataDir = "/home/profile"
headedFallback = false
searchEngine = "google"
`)

	cwd := t.TempDir()
	writeConfig(t, cwd, `
[browser]
headless = true
chromePath = "/project/chrome"
userDataDir = "/project/profile"
headedFallback = true
searchEngine = "ddg"
`)
	trust(t, cwd)

	cfg := mustLoad(t, cwd)
	if cfg.Browser.Engine != "chrome" {
		t.Errorf("browser.engine = %q, want inherited chrome", cfg.Browser.Engine)
	}
	if cfg.Browser.Headless == nil || *cfg.Browser.Headless != true {
		t.Errorf("browser.headless = %v, want project true", cfg.Browser.Headless)
	}
	if cfg.Browser.ChromePath != "/project/chrome" {
		t.Errorf("browser.chromePath = %q", cfg.Browser.ChromePath)
	}
	if cfg.Browser.RemoteURL != "http://home:9222" {
		t.Errorf("browser.remoteUrl = %q, want inherited home value", cfg.Browser.RemoteURL)
	}
	if cfg.Browser.UserDataDir != "/project/profile" {
		t.Errorf("browser.userDataDir = %q", cfg.Browser.UserDataDir)
	}
	if cfg.Browser.HeadedFallback == nil || *cfg.Browser.HeadedFallback != true {
		t.Errorf("browser.headedFallback = %v, want project true", cfg.Browser.HeadedFallback)
	}
	if cfg.Browser.SearchEngine != "ddg" {
		t.Errorf("browser.searchEngine = %q, want ddg", cfg.Browser.SearchEngine)
	}
}

func TestResolveAPIKey(t *testing.T) {
	if (Config{APIKey: "inline"}).ResolveAPIKey() != "inline" {
		t.Error("inline apiKey not returned")
	}
	t.Setenv("KLAUDIA_TEST_KEY", "from-env")
	if (Config{APIKeyEnv: "KLAUDIA_TEST_KEY"}).ResolveAPIKey() != "from-env" {
		t.Error("apiKeyEnv not resolved from environment")
	}
	if (Config{}).ResolveAPIKey() != "" {
		t.Error("empty config should yield empty key")
	}
}

func TestLoadPermissionsAccumulate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	writeConfig(t, home, `
[permissions]
allow = ["Edit"]
deny = ["Bash(rm:*)"]
`)
	cwd := t.TempDir()
	writeConfig(t, cwd, `
[permissions]
allow = ["Bash(go test:*)"]
`)
	trust(t, cwd)

	cfg := mustLoad(t, cwd)
	if len(cfg.Permissions.Allow) != 2 {
		t.Errorf("allow = %v, want home+project merged", cfg.Permissions.Allow)
	}
	if len(cfg.Permissions.Deny) != 1 || cfg.Permissions.Deny[0] != "Bash(rm:*)" {
		t.Errorf("deny = %v", cfg.Permissions.Deny)
	}
}

func TestAppendProjectPermission(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	cwd := t.TempDir()
	trust(t, cwd)
	if ok, err := AppendProjectPermission(cwd, "allow", "Edit"); err != nil || ok {
		t.Fatalf("AppendProjectPermission without .klaudia = %v,%v, want false,nil", ok, err)
	}

	if err := os.MkdirAll(filepath.Join(cwd, ".klaudia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := AppendProjectPermission(cwd, "allow", "Edit"); err != nil || !ok {
		t.Fatalf("AppendProjectPermission allow = %v,%v, want true,nil", ok, err)
	}
	if ok, err := AppendProjectPermission(cwd, "allow", "Edit"); err != nil || !ok {
		t.Fatalf("AppendProjectPermission duplicate = %v,%v, want true,nil", ok, err)
	}
	if ok, err := AppendProjectPermission(cwd, "deny", "Bash(rm:*)"); err != nil || !ok {
		t.Fatalf("AppendProjectPermission deny = %v,%v, want true,nil", ok, err)
	}

	cfg := mustLoad(t, cwd)
	if len(cfg.Permissions.Allow) != 1 || cfg.Permissions.Allow[0] != "Edit" {
		t.Errorf("allow = %v, want [Edit]", cfg.Permissions.Allow)
	}
	if len(cfg.Permissions.Deny) != 1 || cfg.Permissions.Deny[0] != "Bash(rm:*)" {
		t.Errorf("deny = %v, want [Bash(rm:*)]", cfg.Permissions.Deny)
	}
}

// [input] enter must survive Load: merge once dropped the whole Input section,
// so the documented `enter = "newline"` never reached the prompt (#101).
func TestLoadInputEnterProjectOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, "[input]\nenter = \"newline\"\n")

	// Global-only: inherited.
	if cfg := mustLoad(t, t.TempDir()); cfg.Input.Enter != "newline" {
		t.Errorf("input.enter = %q, want newline (from home)", cfg.Input.Enter)
	}

	// Project overrides global.
	cwd := t.TempDir()
	writeConfig(t, cwd, "[input]\nenter = \"send\"\n")
	if cfg := mustLoad(t, cwd); cfg.Input.Enter != "send" {
		t.Errorf("input.enter = %q, want send (project wins)", cfg.Input.Enter)
	}

	// A project config without [input] keeps the home value.
	other := t.TempDir()
	writeConfig(t, other, "theme = \"nord\"\n")
	if cfg := mustLoad(t, other); cfg.Input.Enter != "newline" {
		t.Errorf("input.enter = %q, want newline (inherited past a project config)", cfg.Input.Enter)
	}
}

func mustLoad(t *testing.T, cwd string) Config {
	t.Helper()
	cfg, err := Load(cwd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadSyntaxErrorNamesFileAndLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	cwd := t.TempDir()
	writeConfig(t, cwd, "model = \"sonnet\"\n[sandbox\nmode = \"os\"\n")

	_, err := Load(cwd)
	if err == nil {
		t.Fatal("Load accepted a file that does not parse")
	}
	want := ProjectPath(cwd) + ":2:"
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q, want it to start with %q", err, want)
	}
}

func TestLoadHomeParseErrorFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, "model = 5\n")

	_, err := Load(t.TempDir())
	want := filepath.Join(home, ".klaudia", "config.toml") + ":1:"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %v, want one starting with %q", err, want)
	}
}

func TestLoadUnpositionedErrorNamesFile(t *testing.T) {
	// go-toml reports a duplicate key without a position; the file must
	// still be named.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	cwd := t.TempDir()
	writeConfig(t, cwd, "model = \"a\"\nmodel = \"b\"\n")

	_, err := Load(cwd)
	if err == nil || !strings.HasPrefix(err.Error(), ProjectPath(cwd)+":") {
		t.Errorf("error %v, want one naming %s", err, ProjectPath(cwd))
	}
}

func TestLoadUnknownKeysWarnAndKnownKeysApply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, "theme = \"nord\"\nfutureSetting = true\n")
	cwd := t.TempDir()
	writeConfig(t, cwd, "modle = \"x\"\nmodel = \"sonnet\"\n\n[sandbox]\nmdoe = \"os\"\n")

	cfg := mustLoad(t, cwd)
	if cfg.Model != "sonnet" || cfg.Theme != "nord" {
		t.Errorf("model %q theme %q, want the known keys applied", cfg.Model, cfg.Theme)
	}
	homePath := filepath.Join(home, ".klaudia", "config.toml")
	want := []string{
		homePath + `:2: unknown key "futureSetting", ignored`,
		ProjectPath(cwd) + `:1: unknown key "modle", ignored`,
		ProjectPath(cwd) + `:5: unknown key "sandbox.mdoe", ignored`,
	}
	if !reflect.DeepEqual(cfg.Warnings, want) {
		t.Errorf("warnings =\n%q\nwant\n%q", cfg.Warnings, want)
	}
}

func TestAppendProjectPermissionRefusesBrokenFile(t *testing.T) {
	cwd := t.TempDir()
	writeConfig(t, cwd, "[permissions\n")
	if _, err := AppendProjectPermission(cwd, "allow", "Edit"); err == nil {
		t.Fatal("AppendProjectPermission rewrote a file that does not parse")
	}
	data, _ := os.ReadFile(ProjectPath(cwd))
	if string(data) != "[permissions\n" {
		t.Errorf("file changed to %q", data)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	cfg := mustLoad(t, t.TempDir())
	if cfg.Provider != "" {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

func TestExtraHeadersEnvMergeOverlaysPerKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	writeConfig(t, home, `provider = "openai"
baseURL = "https://x/v1"
extraHeadersEnv = { "CF-Access-Client-Id" = "CF_ID_HOME", "X-Extra" = "X_HOME" }
`)
	cwd := t.TempDir()
	writeConfig(t, cwd, `extraHeadersEnv = { "CF-Access-Client-Id" = "CF_ID_PROJECT", "CF-Access-Client-Secret" = "CF_SECRET" }
`)
	trust(t, cwd)

	cfg := mustLoad(t, cwd)
	// Project overrides the shared key; home-only key survives; project-only key is added.
	want := map[string]string{
		"CF-Access-Client-Id":     "CF_ID_PROJECT",
		"X-Extra":                 "X_HOME",
		"CF-Access-Client-Secret": "CF_SECRET",
	}
	if !reflect.DeepEqual(cfg.ExtraHeadersEnv, want) {
		t.Fatalf("merged extraHeadersEnv = %v, want %v", cfg.ExtraHeadersEnv, want)
	}
}

func TestResolveExtraHeaders(t *testing.T) {
	// Absent -> nil, nil.
	if h, m := (Config{}).ResolveExtraHeaders(); h != nil || m != nil {
		t.Fatalf("empty config: headers=%v missing=%v, want nil,nil", h, m)
	}
	cfg := Config{ExtraHeadersEnv: map[string]string{
		"CF-Access-Client-Id":     "CF_ID",
		"CF-Access-Client-Secret": "CF_SECRET",
	}}
	t.Setenv("CF_ID", "id-123")
	os.Unsetenv("CF_SECRET") // referenced but unset -> reported missing (name only)

	headers, missing := cfg.ResolveExtraHeaders()
	if headers["CF-Access-Client-Id"] != "id-123" {
		t.Errorf("resolved header = %q, want id-123", headers["CF-Access-Client-Id"])
	}
	if _, ok := headers["CF-Access-Client-Secret"]; ok {
		t.Errorf("unset var must not appear in headers: %v", headers)
	}
	if len(missing) != 1 || missing[0] != "CF_SECRET" {
		t.Errorf("missing = %v, want [CF_SECRET]", missing)
	}
}

func TestSessionMaxAge(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultAutoResumeMaxAge, false},
		{"0", 0, false},
		{"12h", 12 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"soon", DefaultAutoResumeMaxAge, true},
		{"-1h", DefaultAutoResumeMaxAge, true},
		{"xd", DefaultAutoResumeMaxAge, true},
	} {
		got, err := Session{AutoResumeMaxAge: tc.in}.MaxAge()
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("MaxAge(%q) = %s, %v; want %s, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestLoadSessionProjectOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, "[session]\nautoResumeMaxAge = \"12h\"\n")
	if cfg := mustLoad(t, t.TempDir()); cfg.Session.AutoResumeMaxAge != "12h" {
		t.Errorf("autoResumeMaxAge = %q, want 12h (from home)", cfg.Session.AutoResumeMaxAge)
	}
	cwd := t.TempDir()
	writeConfig(t, cwd, "[session]\nautoResumeMaxAge = \"0\"\n")
	if cfg := mustLoad(t, cwd); cfg.Session.AutoResumeMaxAge != "0" {
		t.Errorf("autoResumeMaxAge = %q, want 0 (project wins)", cfg.Session.AutoResumeMaxAge)
	}
}

func TestLoadUserHooksParsed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, `
[[hooks.PreToolUse]]
matcher = "Bash"
[[hooks.PreToolUse.hooks]]
type = "command"
command = "echo hi"
timeout = 30

[[hooks.Stop]]
[[hooks.Stop.hooks]]
command = "echo stop"
`)

	cfg := mustLoad(t, t.TempDir())
	if len(cfg.Hooks.PreToolUse) != 1 {
		t.Fatalf("PreToolUse groups = %d, want 1", len(cfg.Hooks.PreToolUse))
	}
	g := cfg.Hooks.PreToolUse[0]
	if g.Matcher != "Bash" || len(g.Hooks) != 1 {
		t.Fatalf("group = %+v", g)
	}
	if h := g.Hooks[0]; h.Type != "command" || h.Command != "echo hi" || h.Timeout != 30 {
		t.Errorf("hook = %+v", h)
	}
	if len(cfg.Hooks.Stop) != 1 {
		t.Errorf("Stop groups = %d, want 1", len(cfg.Hooks.Stop))
	}
}

// Project-level hooks are a code-execution vector and must be dropped: only
// user-level (~/.klaudia) hooks survive Load.
func TestLoadDropsProjectHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	writeConfig(t, home, `
[[hooks.PreToolUse]]
[[hooks.PreToolUse.hooks]]
command = "user-hook"
`)

	cwd := t.TempDir()
	writeConfig(t, cwd, `
[[hooks.PreToolUse]]
[[hooks.PreToolUse.hooks]]
command = "project-hook"

[[hooks.PostToolUse]]
[[hooks.PostToolUse.hooks]]
command = "project-post"
`)

	cfg := mustLoad(t, cwd)
	if len(cfg.Hooks.PreToolUse) != 1 {
		t.Fatalf("PreToolUse groups = %d, want 1 (user only)", len(cfg.Hooks.PreToolUse))
	}
	if got := cfg.Hooks.PreToolUse[0].Hooks[0].Command; got != "user-hook" {
		t.Errorf("surviving hook = %q, want user-hook", got)
	}
	if len(cfg.Hooks.PostToolUse) != 0 {
		t.Errorf("PostToolUse groups = %d, want 0 (project dropped)", len(cfg.Hooks.PostToolUse))
	}
}

func TestBedrockConfigFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	writeConfig(t, home, `provider = "bedrock"
region = "ap-southeast-2"
model = "au.anthropic.claude-sonnet-4-5-20250929-v1:0"
bedrockBetas = ["context-management-2025-06-27"]
`)
	cfg := mustLoad(t, t.TempDir())
	if cfg.Provider != ProviderBedrock || cfg.Region != "ap-southeast-2" ||
		len(cfg.BedrockBetas) != 1 || cfg.BedrockBetas[0] != "context-management-2025-06-27" {
		t.Errorf("cfg = %+v", cfg)
	}
}
