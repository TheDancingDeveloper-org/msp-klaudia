package hooks

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestMatchMatcher(t *testing.T) {
	cases := []struct {
		matcher, tool string
		want          bool
	}{
		{"", "Bash", true},
		{"*", "Bash", true},
		{"Bash", "Bash", true},
		{"Bash", "Edit", false},
		{"Edit|Write", "Write", true},
		{"Edit|Write", "Read", false},
		{"Notebook.*", "NotebookEdit", true},
		{"Notebook.*", "Edit", false},
		// A matcher that is not a valid regexp falls back to exact equality.
		{"(unclosed", "(unclosed", true},
		{"(unclosed", "Bash", false},
	}
	for _, c := range cases {
		if got := matchMatcher(c.matcher, c.tool); got != c.want {
			t.Errorf("matchMatcher(%q, %q) = %v, want %v", c.matcher, c.tool, got, c.want)
		}
	}
}

// TestStdinPayloadShape checks that the JSON written to a hook's stdin carries
// the event name and the event-specific fields.
func TestStdinPayloadShape(t *testing.T) {
	// A hook that echoes its stdin back verbatim on stdout as additionalContext
	// would double-encode; instead capture it to a JSON field the test reads.
	r := New(Config{
		PreToolUse: []Group{{Hooks: []Hook{{
			Command: `cat > "$TMPDIR_HOOK/payload.json"`,
		}}}},
	}, "")
	if r == nil {
		t.Fatal("New returned nil for non-empty config")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR_HOOK", dir)

	_, err := r.Run(context.Background(), PreToolUse, Input{
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"ls"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(dir + "/payload.json")
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatalf("unmarshal payload %q: %v", data, err)
	}
	if in.HookEventName != "PreToolUse" {
		t.Errorf("hook_event_name = %q, want PreToolUse", in.HookEventName)
	}
	if in.ToolName != "Bash" {
		t.Errorf("tool_name = %q, want Bash", in.ToolName)
	}
	if string(in.ToolInput) != `{"command":"ls"}` {
		t.Errorf("tool_input = %q, want the command json", in.ToolInput)
	}
}

func TestDecisionBlockViaStdoutJSON(t *testing.T) {
	r := New(Config{
		PreToolUse: []Group{{Matcher: "Bash", Hooks: []Hook{{
			Command: `echo '{"decision":"block","reason":"nope"}'`,
		}}}},
	}, "")
	dec, err := r.Run(context.Background(), PreToolUse, Input{ToolName: "Bash"})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Block || dec.Reason != "nope" {
		t.Fatalf("got block=%v reason=%q, want block=true reason=nope", dec.Block, dec.Reason)
	}
}

// A hook whose matcher does not match the tool must not run at all.
func TestMatcherGatesExecution(t *testing.T) {
	r := New(Config{
		PreToolUse: []Group{{Matcher: "Edit", Hooks: []Hook{{
			Command: `echo '{"decision":"block","reason":"should not fire"}'`,
		}}}},
	}, "")
	dec, err := r.Run(context.Background(), PreToolUse, Input{ToolName: "Bash"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Block {
		t.Fatalf("hook fired for non-matching tool: %+v", dec)
	}
}

func TestExitCode2Blocks(t *testing.T) {
	r := New(Config{
		Stop: []Group{{Hooks: []Hook{{
			Command: `echo "keep going" >&2; exit 2`,
		}}}},
	}, "")
	dec, err := r.Run(context.Background(), Stop, Input{})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Block || dec.Reason != "keep going" {
		t.Fatalf("got block=%v reason=%q, want block=true reason='keep going'", dec.Block, dec.Reason)
	}
}

// A non-zero exit other than 2 is a non-blocking error: logged, not a block.
func TestOtherNonZeroIsNonBlocking(t *testing.T) {
	var logged int
	r := New(Config{
		PreToolUse: []Group{{Hooks: []Hook{{Command: `echo boom >&2; exit 1`}}}},
	}, "")
	r.Logf = func(string, ...any) { logged++ }
	dec, err := r.Run(context.Background(), PreToolUse, Input{ToolName: "Bash"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Block {
		t.Fatalf("exit 1 must not block, got %+v", dec)
	}
	if logged == 0 {
		t.Error("expected the non-blocking failure to be logged")
	}
}

func TestUserPromptSubmitAdditionalContext(t *testing.T) {
	// JSON form.
	r := New(Config{
		UserPromptSubmit: []Group{{Hooks: []Hook{{
			Command: `echo '{"additionalContext":"from-json"}'`,
		}}}},
	}, "")
	dec, err := r.Run(context.Background(), UserPromptSubmit, Input{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.AdditionalContext != "from-json" {
		t.Errorf("additionalContext = %q, want from-json", dec.AdditionalContext)
	}

	// Nested hookSpecificOutput form.
	r = New(Config{
		UserPromptSubmit: []Group{{Hooks: []Hook{{
			Command: `echo '{"hookSpecificOutput":{"additionalContext":"nested"}}'`,
		}}}},
	}, "")
	dec, _ = r.Run(context.Background(), UserPromptSubmit, Input{Prompt: "hi"})
	if dec.AdditionalContext != "nested" {
		t.Errorf("nested additionalContext = %q, want nested", dec.AdditionalContext)
	}

	// Plain (non-JSON) stdout is taken as context for UserPromptSubmit.
	r = New(Config{
		UserPromptSubmit: []Group{{Hooks: []Hook{{Command: `echo plain-text`}}}},
	}, "")
	dec, _ = r.Run(context.Background(), UserPromptSubmit, Input{Prompt: "hi"})
	if dec.AdditionalContext != "plain-text" {
		t.Errorf("plain additionalContext = %q, want plain-text", dec.AdditionalContext)
	}
}

func TestTimeout(t *testing.T) {
	var logged int
	r := New(Config{
		PreToolUse: []Group{{Hooks: []Hook{{Command: `sleep 5`, Timeout: 1}}}},
	}, "")
	r.Logf = func(string, ...any) { logged++ }
	start := time.Now()
	dec, err := r.Run(context.Background(), PreToolUse, Input{ToolName: "Bash"})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout not honored: took %s", elapsed)
	}
	if dec.Block {
		t.Errorf("a timed-out hook must not block, got %+v", dec)
	}
	if logged == 0 {
		t.Error("expected the timeout to be logged as a non-blocking failure")
	}
}

func TestNilRunnerIsNoop(t *testing.T) {
	var r *Runner
	dec, err := r.Run(context.Background(), PreToolUse, Input{ToolName: "Bash"})
	if err != nil || dec.Block || dec.AdditionalContext != "" {
		t.Fatalf("nil runner should be a no-op, got dec=%+v err=%v", dec, err)
	}
}

func TestNewReturnsNilWhenEmpty(t *testing.T) {
	if r := New(Config{}, ""); r != nil {
		t.Errorf("New(empty) = %v, want nil", r)
	}
}

// TestAggregatesMultipleHooks confirms reasons and contexts from several hooks
// in one event combine, and that any block wins.
func TestAggregatesMultipleHooks(t *testing.T) {
	r := New(Config{
		PreToolUse: []Group{
			{Hooks: []Hook{{Command: `echo '{"additionalContext":"ctx1"}'`}}},
			{Hooks: []Hook{{Command: `echo '{"decision":"block","reason":"blocked"}'`}}},
		},
	}, "")
	dec, err := r.Run(context.Background(), PreToolUse, Input{ToolName: "Bash"})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Block {
		t.Error("expected block from second hook")
	}
	if dec.Reason != "blocked" {
		t.Errorf("reason = %q, want blocked", dec.Reason)
	}
	if dec.AdditionalContext != "ctx1" {
		t.Errorf("additionalContext = %q, want ctx1", dec.AdditionalContext)
	}
}
