package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenthread-ai/klaudia/internal/fakeapi"
)

// Background sub-agents (Agent with background=true) report "on a later turn".
// Only the TUI used to collect them, so over -p and stream-json the result was
// never delivered, and -p exited while the agents still ran (#276). The
// children's requests are routed by a marker in their prompt (fakeapi.Route),
// because they and the parent call the model in no fixed order.

// backgroundAgent is an Agent tool call that launches an Explore sub-agent in
// the background with prompt.
func backgroundAgent(prompt, description string) fakeapi.ToolCall {
	return fakeapi.ToolCall{Name: "Agent", Input: map[string]any{
		"subagent_type": "Explore",
		"prompt":        prompt,
		"description":   description,
		"background":    true,
	}}
}

// parentRequests are the requests that belong to the parent conversation —
// those whose first message is the user's prompt rather than a child's.
func parentRequests(m *FakeModel, prompt string) []Request {
	var out []Request
	for _, r := range m.Requests() {
		if strings.Contains(r.FirstMessage(), prompt) {
			out = append(out, r)
		}
	}
	return out
}

// The issue's repro: two background agents launched in one turn, a parent that
// ends its turn promising to report both. The run must wait for them, hand
// both results to the model in a follow-up turn, and only then print result.
func TestHeadlessDeliversBackgroundSubagents(t *testing.T) {
	m := NewFakeModel(t,
		Turn{Tools: []fakeapi.ToolCall{
			backgroundAgent("COUNT-IN-A: count the files", "count a"),
			backgroundAgent("COUNT-IN-B: count the files", "count b"),
		}},
		Say("Both launched; I will report the counts when they come back."),
		Say("A has 3 files and B has 5."),
	)
	// Slow enough that neither finishes before the parent's turn ends, so the
	// results can only arrive through the drain.
	m.Route("COUNT-IN-A", Turn{Text: "A-RESULT: 3 files", Delay: 300 * time.Millisecond})
	m.Route("COUNT-IN-B", Turn{Text: "B-RESULT: 5 files", Delay: 600 * time.Millisecond})
	e := NewEnv(t, m)

	r := e.Headless("USER-PROMPT launch both and report", "--max-turns", "10")
	if r.ExitCode != 0 {
		t.Fatalf("exit %d\n%s", r.ExitCode, r.dump())
	}
	res := r.ResultEvent()
	if res["result"] != "A has 3 files and B has 5." || res["subtype"] != "success" {
		t.Fatalf("result = %v\n%s", res, r.dump())
	}
	if res["num_turns"] != float64(3) {
		t.Errorf("num_turns = %v, want 3 (both turns of the first run plus the follow-up)", res["num_turns"])
	}

	parent := parentRequests(m, "USER-PROMPT")
	if len(parent) != 3 {
		t.Fatalf("parent called the model %d times, want 3", len(parent))
	}
	last := parent[2].Raw()
	for _, want := range []string{"A-RESULT: 3 files", "B-RESULT: 5 files", "agent-1", "agent-2"} {
		if !strings.Contains(last, want) {
			t.Errorf("follow-up request is missing %q", want)
		}
	}
	if strings.Contains(r.Stdout, `"type":"warning"`) {
		t.Errorf("a delivered run warned:\n%s", r.dump())
	}
}

// With the turn cap already spent there is no turn left to deliver in. The
// run must not exceed --max-turns to deliver, and must say what it dropped.
func TestHeadlessBackgroundAtMaxTurnsWarnsAndExits(t *testing.T) {
	m := NewFakeModel(t, Turn{Tools: []fakeapi.ToolCall{backgroundAgent("SLOW-CHILD: dig", "dig deep")}})
	m.Route("SLOW-CHILD", Turn{Text: "too late", Delay: 2 * time.Second})
	e := NewEnv(t, m)

	r := e.Headless("USER-PROMPT launch it", "--max-turns", "1")
	if r.ExitCode != 3 { // ExitMaxTurns
		t.Fatalf("exit %d, want 3 (max turns)\n%s", r.ExitCode, r.dump())
	}
	var warning string
	for _, ev := range r.Events() {
		if ev["type"] == "warning" {
			warning, _ = ev["content"].(string)
		}
	}
	if !strings.Contains(warning, "agent-1") || !strings.Contains(warning, "dig deep") || !strings.Contains(warning, "--max-turns") {
		t.Errorf("warning = %q, want it to name agent-1 and say --max-turns stopped it\n%s", warning, r.dump())
	}
	if got := len(parentRequests(m, "USER-PROMPT")); got != 1 {
		t.Errorf("parent called the model %d times past --max-turns 1", got)
	}
	if r.Elapsed > 10*time.Second {
		t.Errorf("run took %s: it waited on an agent it could not deliver", r.Elapsed)
	}
}

// Over stream-json the session outlives the turn, so the result is delivered
// at the next turn the client starts — it used to be dropped there too.
func TestStreamJSONDeliversBackgroundSubagentNextTurn(t *testing.T) {
	m := NewFakeModel(t,
		Use("Agent", backgroundAgent("COUNT-IN-C: count the files", "count c").Input),
		Say("launched"),
		Say("it found 7"),
	)
	m.Route("COUNT-IN-C", Say("C-RESULT: 7 files"))
	e := NewEnv(t, m)
	s := e.StartStream()

	s.SendUser("USER-PROMPT launch it")
	if res := s.Next("result"); res["result"] != "launched" {
		t.Fatalf("first result = %v", res)
	}
	// Wait for the child to have been answered, then for it to finish.
	deadline := time.Now().Add(streamWait)
	for len(m.Requests()) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	s.SendUser("what did it find")
	if res := s.Next("result"); res["result"] != "it found 7" {
		t.Fatalf("second result = %v", res)
	}
	if code := s.Close(); code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, s.stderr.String())
	}
	parent := parentRequests(m, "USER-PROMPT")
	if len(parent) != 3 {
		t.Fatalf("parent called the model %d times, want 3", len(parent))
	}
	if !strings.Contains(parent[2].Raw(), "C-RESULT: 7 files") {
		t.Errorf("the next turn's request does not carry the background result:\n%s", parent[2].Raw())
	}
}

// A background writer launched with working_dir inside an additional directory
// writes into that repository, not the session's. This is the path the unit
// tests cannot see: the --add-dir flag has to reach the validation, and the
// child's checkout has to be cut from the other repository.
func TestBackgroundWriterHonoursWorkingDir(t *testing.T) {
	e := NewEnv(t, nil)
	initRepo(t, e.Dir)
	other := filepath.Join(e.Home, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo(t, other)

	m := NewFakeModel(t,
		Turn{Tools: []fakeapi.ToolCall{{Name: "Agent", Input: map[string]any{
			"subagent_type": "general-purpose",
			"prompt":        "WRITE-IN-B: write the file",
			"description":   "write b",
			"background":    true,
			"working_dir":   other,
		}}}},
		Say("launched"),
	)
	m.Route("WRITE-IN-B", Use("Write", map[string]any{
		"file_path": filepath.Join(other, "from-b.txt"),
		"content":   "landed in B\n",
	}), Say("wrote it"))
	e.Model = m

	r := e.Headless("USER-PROMPT write it over there", "--add-dir", other, "--dangerously-skip-permissions")
	if r.ExitCode != 0 {
		t.Fatalf("exit %d\n%s", r.ExitCode, r.dump())
	}
	if got, err := os.ReadFile(filepath.Join(other, "from-b.txt")); err != nil || string(got) != "landed in B\n" {
		t.Fatalf("other repo's file = %q, %v\n%s", got, err, r.dump())
	}
	if _, err := os.Stat(e.Path("from-b.txt")); err == nil {
		t.Fatal("the session's repository received the child's file")
	}
}

// initRepo makes dir a one-commit repository, which is what a working_dir has
// to be before a writer can be cut from it.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.name", "e2e"},
		{"config", "user.email", "e2e@example.invalid"},
		{"commit", "--quiet", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"GIT_CONFIG_NOSYSTEM=1", "HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"),
			"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.invalid",
			"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.invalid",
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
	}
}
