package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/config"
)

// A negative --max-turns used to be accepted and silently treated as
// unlimited. It is a malformed invocation, so it exits as a usage error before
// anything runs.
func TestNegativeMaxTurnsIsAUsageError(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"-p", "hi", "--max-turns", "-5"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if got := exitCodeFor(err); got != ExitUsage {
		t.Fatalf("exit code = %d (err %v), want %d", got, err, ExitUsage)
	}
	if !strings.Contains(err.Error(), "--max-turns -5") {
		t.Errorf("error %q does not name the bad value", err)
	}
}

func TestContinueHasShorthand(t *testing.T) {
	f := NewRootCommand().Flags().ShorthandLookup("c")
	if f == nil || f.Name != "continue" {
		t.Fatalf("-c = %v, want --continue", f)
	}
}

// Cobra hides its completion command on a root with no sub-commands, so the
// help text is the only place a user finds it.
func TestHelpMentionsCompletion(t *testing.T) {
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "klaudia completion") {
		t.Errorf("--help does not mention shell completion:\n%s", out.String())
	}
}

// With apiKeyEnv set, "needs apiKey or apiKeyEnv" alone sends the user looking
// at a config that is already right; the fix is exporting the named variable.
func TestMissingKeyErrorNamesTheVariable(t *testing.T) {
	t.Setenv("KLAUDIA_TEST_UNSET_KEY", "")
	_, _, err := buildProvider(config.Config{
		Provider:  config.ProviderOpenAI,
		BaseURL:   "http://127.0.0.1:9/v1",
		APIKeyEnv: "KLAUDIA_TEST_UNSET_KEY",
	})
	if err == nil || !strings.Contains(err.Error(), "$KLAUDIA_TEST_UNSET_KEY") {
		t.Fatalf("err = %v, want it to name $KLAUDIA_TEST_UNSET_KEY", err)
	}

	_, _, err = buildProvider(config.Config{Provider: config.ProviderOpenAI, BaseURL: "http://127.0.0.1:9/v1"})
	if err == nil || !strings.Contains(err.Error(), "needs apiKey or apiKeyEnv") {
		t.Fatalf("err = %v, want the generic missing-key message", err)
	}
}

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

// `klaudia goal run` used to run "goal run" as a headless prompt (#248).
func TestGoalSubcommandShapedPromptIsAUsageError(t *testing.T) {
	for _, args := range [][]string{{"goal"}, {"goal", "run"}, {"goal", "run", "5"}, {"Goal", "stop"}, {"goal run"}} {
		if !looksLikeGoalSubcommand(args) {
			t.Errorf("%q not caught", args)
		}
	}
	for _, args := range [][]string{{"goal:", "fix", "the", "build"}, {"goal", "run", "the", "tests"}, {"explain", "goal", "run"}, {"run"}} {
		if looksLikeGoalSubcommand(args) {
			t.Errorf("%q is a real prompt but was refused", args)
		}
	}
	e := newCLIEnv(t, newFakeModel(t))
	r := e.run(nil, "goal", "run")
	if r.Err == nil || !isUsageError(r.Err) || !strings.Contains(r.Err.Error(), "--loop") {
		t.Fatalf("err = %v, want a usage error pointing at --loop", r.Err)
	}
}

// --prompt-interactive opens the TUI; every combination that would run
// headless instead is refused before anything starts.
func TestPromptInteractiveRejectsHeadlessCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"--prompt-interactive", "hi", "-p"},
		{"--prompt-interactive", "hi", "also", "positional"},
		{"--prompt-interactive", "hi", "--loop", "--dangerously-skip-permissions"},
		{"--prompt-interactive", "hi", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
		{"--prompt-interactive", "hi", "--input-format", "acp"},
	} {
		m := newFakeModel(t)
		e := newCLIEnv(t, m)
		r := e.run(nil, args...)
		if r.Err == nil || !isUsageError(r.Err) || !strings.Contains(r.Err.Error(), "--prompt-interactive") {
			t.Errorf("%v: err = %v, want a usage error naming --prompt-interactive", args, r.Err)
		}
		if len(m.Requests()) != 0 {
			t.Errorf("%v: the model was called", args)
		}
	}
}
