package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/subagent"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// repeatProvider always asks for the same tool call, modelling a child that
// never decides it is finished.
type repeatProvider struct {
	turn  anthropic.BetaMessage
	calls int
}

func (p *repeatProvider) StreamTurn(_ context.Context, _ anthropic.BetaMessageNewParams, _ api.StreamSink) (anthropic.BetaMessage, error) {
	p.calls++
	return p.turn, nil
}

func readOnlySpawner(t *testing.T, provider api.Provider, dir string, maxTurns int) *Spawner {
	t.Helper()
	read, err := tools.NewRead()
	if err != nil {
		t.Fatal(err)
	}
	return NewSpawner(provider, tools.NewRegistry(read), "claude-opus-4-8",
		bypassPerm(), nil, maxTurns).WithWorkingDir(dir)
}

func fixtureFile(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

// The reported bug: a sub-agent ran for twenty minutes emitting nothing, so the
// user could not tell work from a hang. The child loop was given a nil emitter,
// making it structurally incapable of reporting. Its tool calls must now reach
// the caller as they happen.
func TestSpawnRelaysChildToolCallsAsProgress(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path}),
	}}

	var lines []string
	_, _, err := readOnlySpawner(t, provider, dir, 0).
		Spawn(context.Background(), nil, "Explore", "read it", func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if len(lines) == 0 {
		t.Fatal("no progress reported — the sub-agent is still a silent box")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Read") {
		t.Errorf("progress does not name the child's tool:\n%s", joined)
	}
	if !strings.Contains(joined, "notes.txt") {
		t.Errorf("progress does not identify what the tool acted on:\n%s", joined)
	}
}

// Headless callers pass nil; that must stay a silent no-op, not a panic.
func TestSpawnWithoutProgressIsSafe(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path}),
	}}
	if _, _, err := readOnlySpawner(t, provider, dir, 0).
		Spawn(context.Background(), nil, "Explore", "read it", nil); err != nil {
		t.Fatalf("Spawn with nil progress: %v", err)
	}
}

// A child that never stops must still terminate: --max-turns defaults to 0
// ("unlimited"), which is defensible for the main loop the user is watching and
// not for an invisible child. The bound applies and the truncation is declared
// rather than passed off as a finished answer.
func TestSpawnIsBoundedWhenNoMaxTurnsIsSet(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &repeatProvider{turn: toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path})}

	out, _, err := readOnlySpawner(t, provider, dir, 0).
		Spawn(context.Background(), nil, "Explore", "loop forever", nil)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if provider.calls > defaultSubagentMaxTurns+1 {
		t.Errorf("child ran %d turns, past the %d-turn bound", provider.calls, defaultSubagentMaxTurns)
	}
	if !strings.Contains(out, "turn limit") {
		t.Errorf("hitting the limit was not declared to the caller: %q", out)
	}
}

// An explicit --max-turns still wins over the default.
func TestSpawnHonoursExplicitMaxTurns(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &repeatProvider{turn: toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path})}
	if _, _, err := readOnlySpawner(t, provider, dir, 2).
		Spawn(context.Background(), nil, "Explore", "loop", nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls > 3 {
		t.Errorf("explicit max-turns ignored: %d calls", provider.calls)
	}
}

// A type that names its own maxTurns stops there even when the session's
// bound is higher. The type's author wrote the bound for a reason.
func TestSpawnHonoursTheTypesMaxTurns(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &repeatProvider{turn: toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path})}
	s := readOnlySpawner(t, provider, dir, 50).WithTypes([]subagent.Type{{
		Name: "Reviewer", Tools: []string{"Read"}, MaxTurns: 2,
	}})
	if _, _, err := s.Spawn(context.Background(), nil, "Reviewer", "loop", nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls > 3 {
		t.Errorf("the type's maxTurns was ignored: %d calls", provider.calls)
	}
}

// The same bound holds when it comes from a file rather than a constructed
// type: maxTurns: 2 in frontmatter stops the child at 2.
func TestSpawnHonoursLoadedMaxTurns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	dir, path := fixtureFile(t)
	os.MkdirAll(filepath.Join(dir, ".klaudia", "agents"), 0o755)
	os.WriteFile(filepath.Join(dir, ".klaudia", "agents", "reviewer.md"), []byte(
		"---\nname: reviewer\ndescription: reviews\nmaxTurns: 2\n---\nReview."), 0o644)
	provider := &repeatProvider{turn: toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path})}
	s := readOnlySpawner(t, provider, dir, 50).WithTypes(subagent.Load(dir, nil))
	if _, _, err := s.Spawn(context.Background(), nil, "reviewer", "loop", nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls > 3 {
		t.Errorf("loaded maxTurns was ignored: %d calls", provider.calls)
	}
}

// A tool the frontmatter disallows is not in the registry the child runs
// with, even when the type otherwise grants everything.
func TestLoadedDisallowedToolIsAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".klaudia", "agents"), 0o755)
	os.WriteFile(filepath.Join(dir, ".klaudia", "agents", "safe.md"), []byte(
		"---\nname: safe\ndescription: no bash\ndisallowedTools: [Bash]\n---\nBe careful."), 0o644)
	loaded, ok := subagent.Find(subagent.Load(dir, nil), "safe")
	if !ok {
		t.Fatal("safe was not loaded")
	}
	read, err := tools.NewRead()
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Filter(tools.NewRegistry(read, namedStub("Bash")))
	if _, ok := got.Lookup("Bash"); ok {
		t.Error("Bash survived disallowedTools")
	}
	if _, ok := got.Lookup("Read"); !ok {
		t.Error("Read was removed along with Bash")
	}
}

// With no session bound the ceiling is the shared default, not whatever the
// call or the type file asked for.
func TestChildMaxTurnsCapsAtTheDefault(t *testing.T) {
	s := &Spawner{}
	if got := s.childMaxTurns(ChildSpec{RequestedMaxTurns: 1000}, subagent.Type{}); got != defaultSubagentMaxTurns {
		t.Errorf("requested 1000 on a default spawner gave %d, want %d", got, defaultSubagentMaxTurns)
	}
	if got := s.childMaxTurns(ChildSpec{}, subagent.Type{MaxTurns: 1000}); got != defaultSubagentMaxTurns {
		t.Errorf("type maxTurns 1000 on a default spawner gave %d, want %d", got, defaultSubagentMaxTurns)
	}
	// A tighter request still wins, and a session bound still caps both.
	if got := s.childMaxTurns(ChildSpec{RequestedMaxTurns: 3}, subagent.Type{MaxTurns: 1000}); got != 3 {
		t.Errorf("a tighter request gave %d, want 3", got)
	}
	s.maxTurns = 20
	if got := s.childMaxTurns(ChildSpec{RequestedMaxTurns: 1000}, subagent.Type{MaxTurns: 1000}); got != 20 {
		t.Errorf("the session ceiling gave %d, want 20", got)
	}
}

func TestSubagentProgressLine(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"tool with path", Event{Type: "tool_use", ToolName: "Read",
			Input: map[string]any{"file_path": "/tmp/x.go"}}, "Read /tmp/x.go"},
		{"tool with command", Event{Type: "tool_use", ToolName: "Bash",
			Input: map[string]any{"command": "go test ./..."}}, "Bash go test ./..."},
		{"tool with no known field", Event{Type: "tool_use", ToolName: "Jobs",
			Input: map[string]any{"unknown": 1}}, "Jobs"},
		// The child's prose is its working-out, not progress.
		{"assistant text ignored", Event{Type: "assistant", Text: "thinking"}, ""},
		{"result ignored", Event{Type: "tool_result", ToolName: "Read"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := subagentProgressLine(c.ev); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A deep path keeps its filename: clipping the front leaves the part that says
// which file, clipping the end would leave an unreadable prefix.
func TestLongPathKeepsItsFilename(t *testing.T) {
	long := "/var/folders/b7/47_wgj290yj9jjkqc96jjv480000gn/T/TestSpawnRelays123/notes.txt"
	got := subagentProgressLine(Event{Type: "tool_use", ToolName: "Read",
		Input: map[string]any{"file_path": long}})
	if !strings.Contains(got, "notes.txt") {
		t.Errorf("filename clipped away: %q", got)
	}
}

// A long argument must not run away with the display, and newlines must not
// break the one-line-per-call shape.
func TestProgressLineIsClippedToOneLine(t *testing.T) {
	got := subagentProgressLine(Event{Type: "tool_use", ToolName: "Bash",
		Input: map[string]any{"command": "echo " + strings.Repeat("x", 500) + "\nsecond line"}})
	if strings.Contains(got, "\n") {
		t.Errorf("progress line contains a newline: %q", got)
	}
	if len([]rune(got)) > progressFieldLimit+len("Bash ") {
		t.Errorf("progress line not clipped: %d runes", len([]rune(got)))
	}
}
