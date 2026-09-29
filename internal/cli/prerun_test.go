package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// headless runs one launch in cwd with an isolated home and no credentials in
// the environment, returning stdout and the exit code. (The error message
// itself is printed to the process's stderr by ExecuteContext, not here.)
func headless(t *testing.T, cwd string, args ...string) (stdout string, code int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Chdir(cwd)
	var out bytes.Buffer
	cmd := NewRootCommand()
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	code = exitCodeFor(cmd.ExecuteContext(context.Background()))
	return out.String(), code
}

// lastResult decodes the final stdout line as a result message.
func lastResult(t *testing.T, stdout string) ResultMessage {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var res ResultMessage
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("last stdout line is not a result: %v\nstdout:\n%s", err, stdout)
	}
	if res.Type != "result" {
		t.Fatalf("last stdout line has type %q, want result:\n%s", res.Type, stdout)
	}
	return res
}

// incompleteOpenAI is a project whose config names the openai provider but
// no endpoint, so the run fails while building the provider.
func incompleteOpenAI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".klaudia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".klaudia", "config.toml"), []byte("provider = \"openai\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A script parsing stdout used to get nothing when the run could not start:
// the reason went to stderr as plain text. In the JSON modes it is now a
// result line, and the exit code is unchanged.
func TestPreRunFailureIsAJSONResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		cwd  func(*testing.T) string
		args []string
		want string
	}{
		{"missing credential, json", func(t *testing.T) string { return t.TempDir() },
			[]string{"-p", "hi", "--output-format", "json"}, "needs credentials"},
		{"missing credential, stream-json", func(t *testing.T) string { return t.TempDir() },
			[]string{"-p", "hi", "--output-format", "stream-json", "--verbose"}, "needs credentials"},
		{"missing credential, embedding", func(t *testing.T) string { return t.TempDir() },
			[]string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, "needs credentials"},
		{"incomplete provider config", incompleteOpenAI,
			[]string{"-p", "hi", "--output-format", "json"}, "requires baseURL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, code := headless(t, tc.cwd(t), tc.args...)
			res := lastResult(t, stdout)
			if !res.IsError || res.Subtype != "error_during_execution" {
				t.Errorf("is_error = %v, subtype = %q; want an error result", res.IsError, res.Subtype)
			}
			if !strings.HasPrefix(res.Result, "Error: ") || !strings.Contains(res.Result, tc.want) {
				t.Errorf("result = %q, want the reason (%q)", res.Result, tc.want)
			}
			if res.SessionID == "" {
				t.Error("the session had been chosen, but the result does not name it")
			}
			if res.NumTurns != 0 || res.DurationAPIMS != 0 {
				t.Errorf("num_turns = %d, duration_api_ms = %d; no request was made", res.NumTurns, res.DurationAPIMS)
			}
			if code != ExitError {
				t.Errorf("exit code = %d, want %d", code, ExitError)
			}
		})
	}
}

// Text mode has no result line to put a failure in; stdout stays empty.
func TestPreRunFailureInTextModeLeavesStdoutEmpty(t *testing.T) {
	stdout, code := headless(t, t.TempDir(), "-p", "hi")
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
}

// A usage error found once the format is known is reported in it too, and
// keeps its usage exit code.
func TestUsageErrorInJSONModeIsAResultAndExits2(t *testing.T) {
	stdout, code := headless(t, t.TempDir(), "-p", "hi", "--output-format", "json", "--new-session", "--continue")
	res := lastResult(t, stdout)
	if !res.IsError || !strings.Contains(res.Result, "--new-session cannot be combined") {
		t.Errorf("result = %+v", res)
	}
	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
}

// Every way of invoking Klaudia wrongly exits 2, not only an unknown flag.
func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus"},
		{"-p", "hi", "--output-format", "yaml"},
		{"--create-config=foo"},
		{"-p", "hi", "--new-session", "--resume", "abc"},
	} {
		stdout, code := headless(t, t.TempDir(), args...)
		if code != ExitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, ExitUsage)
		}
		if strings.Contains(strings.Join(args, " "), "json") && stdout != "" {
			t.Errorf("%v: stdout = %q", args, stdout)
		}
	}
}

// On a completed run duration_api_ms is the time spent on requests, not a
// copy of the wall time. Their being different numbers is not guaranteed on
// a fast fake, so this checks the relation that must hold.
func TestHeadlessJSONResultReportsAPITime(t *testing.T) {
	srv := httptest.NewServer(&fakeChat{})
	defer srv.Close()
	stdout, code := headless(t, workdir(t, srv.URL), "-p", "hi", "--output-format", "json", "--permission-mode", "dontAsk")
	res := lastResult(t, stdout)
	if res.IsError || code != ExitOK {
		t.Fatalf("run failed (exit %d): %+v", code, res)
	}
	if res.DurationAPIMS > res.DurationMS {
		t.Errorf("duration_api_ms = %d exceeds duration_ms = %d", res.DurationAPIMS, res.DurationMS)
	}
}
