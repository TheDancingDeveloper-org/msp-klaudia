package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/gitguard"
	"github.com/greenthread-ai/klaudia/internal/sandbox"
	"github.com/greenthread-ai/klaudia/internal/subagent"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// waitFor polls cond until true or the deadline, so a test never blocks forever
// on a background goroutine that misbehaves.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func backgroundSpawner(t *testing.T, provider api.Provider, dir string) *Spawner {
	t.Helper()
	read, err := tools.NewRead()
	if err != nil {
		t.Fatal(err)
	}
	write, err := tools.NewWrite()
	if err != nil {
		t.Fatal(err)
	}
	return NewSpawner(provider, tools.NewRegistry(read, write), "claude-opus-4-8",
		bypassPerm(), nil, 0).WithWorkingDir(dir)
}

// A background launch must return a handle promptly and not block on the child.
func TestSpawnBackgroundReturnsHandleImmediately(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path}),
	}}
	s := backgroundSpawner(t, provider, dir)

	id, _, err := s.SpawnBackground("", nil, "Explore", "read it", "read the notes", nil)
	if err != nil {
		t.Fatalf("SpawnBackground: %v", err)
	}
	if id == "" {
		t.Fatal("no handle returned")
	}
	// It is tracked as running (or already done) the moment we hold the id.
	if _, ok := s.Background().Get(id); !ok {
		t.Fatalf("agent %q not registered", id)
	}

	waitFor(t, func() bool {
		a, _ := s.Background().Get(id)
		return a.Done()
	})
	a, _ := s.Background().Get(id)
	if a.Status != BackgroundSucceeded {
		t.Fatalf("status = %q, err = %q", a.Status, a.Err)
	}
	if a.Label != "read the notes" {
		t.Errorf("label = %q", a.Label)
	}
}

// A finished background agent's result is retrievable via the registry and,
// once collected, is not delivered a second time.
func TestBackgroundResultIsDeliveredOnce(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path}),
	}}
	s := backgroundSpawner(t, provider, dir)

	id, _, err := s.SpawnBackground("", nil, "Explore", "read it", "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })

	report := s.Background().PendingReport()
	if !strings.Contains(report, id) || !strings.Contains(report, "succeeded") {
		t.Fatalf("first report missing the finished agent:\n%s", report)
	}
	if again := s.Background().PendingReport(); again != "" {
		t.Errorf("a collected result was delivered again:\n%s", again)
	}
}

// A background writer works in a checkout of its own, and the file it writes
// lands in the parent tree once it finishes. The checkout does not outlive it.
func TestBackgroundWriterLandsInParentTree(t *testing.T) {
	root := gitRepo(t)
	w := &relWriteTool{mu: new(sync.Mutex), rel: "made.txt", body: "from the background child\n"}
	s := writingSpawner(t, w, root).WithWorktrees(true)

	id, _, err := s.SpawnBackground("", nil, "general-purpose", "write it", "rw", nil)
	if err != nil {
		t.Fatalf("SpawnBackground: %v", err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })

	a, _ := s.Background().Get(id)
	if a.Status != BackgroundSucceeded {
		t.Fatalf("status = %q, err = %q", a.Status, a.Err)
	}
	if !a.Isolated {
		t.Error("writer not marked isolated")
	}
	if len(w.wrote()) != 1 || w.wrote()[0] == root {
		t.Fatalf("tool dirs = %v, want a checkout other than %s", w.wrote(), root)
	}
	if _, err := os.Stat(w.wrote()[0]); err == nil {
		t.Errorf("the checkout at %s outlived the sub-agent", w.wrote()[0])
	}
	body, err := os.ReadFile(filepath.Join(root, "made.txt"))
	if err != nil {
		t.Fatalf("the child's work never reached the working tree: %v", err)
	}
	if string(body) != "from the background child\n" {
		t.Errorf("made.txt = %q", body)
	}
	if !strings.Contains(a.Result, "1 file applied to the working tree") {
		t.Errorf("result does not report what landed:\n%s", a.Result)
	}
}

// A conflict keeps the checkout and names it: the parent's copy moved after
// the child started, so applying would overwrite work the child never saw.
func TestBackgroundWriterKeepsCheckoutOnConflict(t *testing.T) {
	root := gitRepo(t)
	if err := os.WriteFile(filepath.Join(root, "made.txt"), []byte("parent's version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &relWriteTool{mu: new(sync.Mutex), rel: "made.txt", body: "child's version\n"}
	s := writingSpawner(t, w, root).WithWorktrees(true)

	id, _, err := s.SpawnBackground("", nil, "general-purpose", "write it", "rw", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The child writes into its checkout before adoption. Wait until it has,
	// then change the parent file so the patch no longer fits.
	waitFor(t, func() bool { return len(w.wrote()) == 1 })
	if err := os.WriteFile(filepath.Join(root, "made.txt"), []byte("parent moved on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })

	a, _ := s.Background().Get(id)
	if a.Status != BackgroundSucceeded {
		t.Fatalf("status = %q, err = %q", a.Status, a.Err)
	}
	if !strings.Contains(a.Result, "NOT applied") || !strings.Contains(a.Result, w.wrote()[0]) {
		t.Errorf("result does not keep and name the checkout:\n%s", a.Result)
	}
	if _, err := os.Stat(w.wrote()[0]); err != nil {
		t.Errorf("conflict removed the checkout: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "made.txt"))
	if err != nil || string(body) != "parent moved on\n" {
		t.Errorf("parent file was overwritten: %q %v", body, err)
	}
}

// A writer whose turn fails keeps its checkout: applying half a change is the
// outcome isolation exists to prevent. The failure names the checkout.
func TestBackgroundWriterKeepsCheckoutOnFailure(t *testing.T) {
	root := gitRepo(t)
	w := &relWriteTool{mu: new(sync.Mutex), rel: "made.txt", body: "half done\n"}
	s := NewSpawner(&errorProvider{}, tools.NewRegistry(w), "claude-opus-4-8",
		bypassPerm(), nil, 2).WithWorkingDir(root).WithWorktrees(true)

	id, _, err := s.SpawnBackground("", nil, "general-purpose", "write it", "rw", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })

	a, _ := s.Background().Get(id)
	if a.Status != BackgroundFailed {
		t.Fatalf("status = %q, want failed", a.Status)
	}
	if !strings.Contains(a.Err, "left in") {
		t.Errorf("error does not keep the checkout: %q", a.Err)
	}
}

// When a checkout cannot be cut, the writer fails rather than silently sharing
// the tree. A file where the worktree directory must be is the ordinary way
// `git worktree add` refuses.
func TestWriterFailsWhenIsolationFails(t *testing.T) {
	root := gitRepo(t)
	blocked := filepath.Join(os.Getenv("KLAUDIA_CONFIG_DIR"), "worktrees")
	if err := os.MkdirAll(filepath.Dir(blocked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &relWriteTool{mu: new(sync.Mutex), rel: "made.txt", body: "x\n"}
	s := writingSpawner(t, w, root).WithWorktrees(true)

	id, _, err := s.SpawnBackground("", nil, "general-purpose", "write", "rw", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })
	a, _ := s.Background().Get(id)
	if a.Status != BackgroundFailed {
		t.Fatalf("status = %q, err = %q, want failed", a.Status, a.Err)
	}
	if !strings.Contains(a.Err, "isolate") {
		t.Errorf("error does not explain the failure: %q", a.Err)
	}
	if len(w.wrote()) != 0 {
		t.Errorf("the writer ran in the shared tree: %v", w.wrote())
	}
	if _, err := os.Stat(filepath.Join(root, "made.txt")); err == nil {
		t.Error("a file landed in the parent tree from a writer that should have failed")
	}
}

// A read-only type shares the parent tree even when isolation is on: a checkout
// would make every path it returns wrong, and there is nothing to isolate.
func TestBackgroundReadOnlySharesTree(t *testing.T) {
	root := gitRepo(t)
	capture := &captureTool{}
	s := NewSpawner(&scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Capture", map[string]any{}),
	}}, tools.NewRegistry(capture), "claude-opus-4-8", bypassPerm(), nil, 2).
		WithWorkingDir(root).WithWorktrees(true).
		WithTypes([]subagent.Type{{Name: "Explore", Tools: []string{"Capture"}}})

	id, _, err := s.SpawnBackground("", nil, "Explore", "look", "ro", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })
	a, _ := s.Background().Get(id)
	if a.Status != BackgroundSucceeded {
		t.Fatalf("status = %q, err = %q", a.Status, a.Err)
	}
	if a.Isolated {
		t.Error("read-only agent marked isolated")
	}
	if capture.got.WorkingDir != root {
		t.Errorf("read-only child ran in %q, want the parent tree %q", capture.got.WorkingDir, root)
	}
}

// A project-defined type is launchable in the background: SpawnBackground
// resolves against the types the spawner was given, not only the built-ins.
func TestBackgroundLaunchesCustomType(t *testing.T) {
	dir := t.TempDir()
	capture := &captureTool{}
	custom := subagent.Type{
		Name:         "reviewer",
		Tools:        []string{"Capture"},
		SystemPrompt: "you review",
	}
	s := NewSpawner(&scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Capture", map[string]any{}),
	}}, tools.NewRegistry(capture), "claude-opus-4-8", bypassPerm(), nil, 2).
		WithWorkingDir(dir).WithTypes([]subagent.Type{custom})

	id, _, err := s.SpawnBackground("", nil, "reviewer", "review it", "custom", nil)
	if err != nil {
		t.Fatalf("SpawnBackground: %v", err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })
	a, _ := s.Background().Get(id)
	if a.Status != BackgroundSucceeded {
		t.Fatalf("status = %q, err = %q", a.Status, a.Err)
	}
	if capture.got.WorkingDir != dir {
		t.Errorf("child ran in %q, want %q", capture.got.WorkingDir, dir)
	}
}

// The background child sees the environment and the project's instructions, not
// only its type's prompt.
func TestBackgroundSystemPromptCarriesProjectContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("always run the tests"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := &captureProvider{t: t}
	s := NewSpawner(seen, tools.NewRegistry(), "claude-opus-4-8", bypassPerm(), nil, 1).
		WithWorkingDir(dir)

	id, _, err := s.SpawnBackground("", nil, "general-purpose", "go", "ctx", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })

	sys := seen.system()
	if !strings.Contains(sys, dir) {
		t.Errorf("system prompt has no working directory:\n%s", sys)
	}
	if !strings.Contains(sys, "always run the tests") {
		t.Errorf("system prompt dropped CLAUDE.md:\n%s", sys)
	}
}

// captureProvider records the system prompt of the one turn it is asked for.
type captureProvider struct {
	t   *testing.T
	mu  sync.Mutex
	sys string
}

func (p *captureProvider) StreamTurn(_ context.Context, params anthropic.BetaMessageNewParams, sink api.StreamSink) (anthropic.BetaMessage, error) {
	var sys string
	for _, block := range params.System {
		sys += block.Text
	}
	p.mu.Lock()
	p.sys = sys
	p.mu.Unlock()
	if sink.OnText != nil {
		sink.OnText("done")
	}
	return textTurn(p.t, "done"), nil
}

func (p *captureProvider) system() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sys
}

// The parent's command guard reaches a background child even though its context
// is detached from the launching turn. A git checkout that would discard
// protected work is refused.
func TestBackgroundChildHonoursCommandGuard(t *testing.T) {
	root := gitRepo(t)
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := gitguard.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	bash, err := tools.NewBash(sandbox.NewLocal())
	if err != nil {
		t.Fatal(err)
	}
	s := NewSpawner(&scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Bash", map[string]any{"command": "git checkout -- notes.txt"}),
	}}, tools.NewRegistry(bash), "claude-opus-4-8", bypassPerm(), nil, 2).
		WithWorkingDir(root).WithWorktrees(false).
		WithCommandGuard(base.CheckTool)

	id, _, err := s.SpawnBackground("", nil, "general-purpose", "tidy up", "guard", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })

	body, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "uncommitted\n" {
		t.Errorf("the guard let the checkout through; notes.txt = %q", body)
	}
}

// Progress lines from a background child reach the callback the launcher
// passed, not only the registry's activity line.
func TestBackgroundProgressIsReported(t *testing.T) {
	dir, path := fixtureFile(t)
	provider := &scriptedProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "tu1", "Read", map[string]any{"file_path": path}),
	}}
	s := backgroundSpawner(t, provider, dir)

	var mu sync.Mutex
	var lines []string
	id, _, err := s.SpawnBackground("", nil, "Explore", "read it", "prog", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.Background().Get(id); return a.Done() })

	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 {
		t.Fatal("progress callback was never called")
	}
	a, _ := s.Background().Get(id)
	if a.Activity == "" && a.Status == BackgroundRunning {
		t.Error("registry activity was not updated")
	}
}

// An unknown type is rejected before anything is registered.
func TestSpawnBackgroundRejectsUnknownType(t *testing.T) {
	s := backgroundSpawner(t, &scriptedProvider{}, t.TempDir())
	if _, _, err := s.SpawnBackground("", nil, "bogus", "x", "l", nil); err == nil {
		t.Fatal("expected an unknown type to be rejected")
	}
	if got := s.Background().List(); len(got) != 0 {
		t.Errorf("a rejected launch still registered: %v", got)
	}
}

// The registry's List/status logic backs the /agents view: running agents show
// as running with a growing elapsed time, finished ones keep their result.
func TestRegistryListReportsStates(t *testing.T) {
	r := NewBackgroundRegistry()
	id1 := r.register("", "Explore", "search", false, "", true, nil)
	id2 := r.register("", "general-purpose", "build", true, "", true, nil)

	r.finish(id1, "found it", nil)
	r.finish(id2, "", fmt.Errorf("boom"))

	byID := map[string]BackgroundAgent{}
	for _, a := range r.List() {
		byID[a.ID] = a
	}
	if byID[id1].Status != BackgroundSucceeded || byID[id1].Result != "found it" {
		t.Errorf("id1 = %+v", byID[id1])
	}
	if byID[id2].Status != BackgroundFailed || byID[id2].Err != "boom" {
		t.Errorf("id2 = %+v", byID[id2])
	}
	if !byID[id2].Isolated {
		t.Error("writer should be marked isolated in the listing")
	}
	// A still-running agent reports a non-negative elapsed and Done()==false.
	id3 := r.register("", "Explore", "slow", false, "", true, nil)
	if a, _ := r.Get(id3); a.Done() || a.Elapsed() < 0 {
		t.Errorf("running agent: done=%v elapsed=%v", a.Done(), a.Elapsed())
	}
}

// Concurrent writers and readers of the registry must be race-free (run under
// -race). This exercises register/finish/setActivity/List/TakeFinished at once.
func TestBackgroundRegistryConcurrency(t *testing.T) {
	r := NewBackgroundRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := r.register("", "Explore", fmt.Sprintf("t%d", n), false, "", true, nil)
			r.setActivity(id, "Read x")
			r.finish(id, "done", nil)
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.List()
			_ = r.TakeFinished()
			_ = r.PendingReport()
		}()
	}
	wg.Wait()
	// Every registered agent eventually collected exactly once across polls.
	seen := map[string]bool{}
	for _, a := range r.List() {
		seen[a.ID] = true
	}
	if len(seen) != 20 {
		t.Errorf("registered %d agents, want 20", len(seen))
	}
}

// The registry is per process and an ACP server runs several conversations in
// one: a result must reach the conversation that launched the agent, and only
// that one (#276).
func TestRegistryDeliversOnlyToLaunchingConversation(t *testing.T) {
	r := NewBackgroundRegistry()
	a := r.register("thread-a", "Explore", "a's search", false, "", true, nil)
	b := r.register("thread-b", "Explore", "b's search", false, "", true, nil)
	r.finish(a, "found in a", nil)
	r.finish(b, "found in b", nil)

	if got := r.PendingReport(); got != "" {
		t.Errorf("the default conversation collected another conversation's result: %q", got)
	}
	gotA := r.PendingReportFor("thread-a")
	if !strings.Contains(gotA, "found in a") || strings.Contains(gotA, "found in b") {
		t.Errorf("thread-a report = %q", gotA)
	}
	if again := r.PendingReportFor("thread-a"); again != "" {
		t.Errorf("thread-a's result delivered twice: %q", again)
	}
	if gotB := r.PendingReportFor("thread-b"); !strings.Contains(gotB, "found in b") {
		t.Errorf("thread-b report = %q", gotB)
	}
}

// Undelivered is what a headless run waits on: running agents, and finished
// ones whose result has not been collected yet. Once collected, nothing is left.
func TestRegistryUndelivered(t *testing.T) {
	r := NewBackgroundRegistry()
	slow := r.register("", "Explore", "slow", false, "", true, nil)
	fast := r.register("", "Explore", "fast", false, "", true, nil)
	_ = r.register("other", "Explore", "elsewhere", false, "", true, nil)
	r.finish(fast, "quick answer", nil)

	running, ready := r.Undelivered("")
	if len(running) != 1 || running[0].ID != slow {
		t.Errorf("running = %+v, want only %s", running, slow)
	}
	if len(ready) != 1 || ready[0].ID != fast {
		t.Errorf("ready = %+v, want only %s", ready, fast)
	}

	_ = r.PendingReport()
	r.finish(slow, "slow answer", nil)
	_ = r.PendingReport()
	if running, ready := r.Undelivered(""); len(running)+len(ready) != 0 {
		t.Errorf("after delivery: running=%v ready=%v", running, ready)
	}
}

// A foreground child is registered while it runs and collected when it returns,
// so /agents can see it without its result also arriving as a background report.
func TestForegroundChildIsRegisteredAndCollected(t *testing.T) {
	dir := t.TempDir()
	provider := &scriptedProvider{turns: []anthropic.BetaMessage{
		{StopReason: "end_turn", Content: []anthropic.BetaContentBlockUnion{{Type: "text", Text: "done"}}},
	}}
	sp := readOnlySpawner(t, provider, dir, 0)
	_, _, err := sp.Spawn(context.Background(), nil, "Explore", "look", nil)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	list := sp.Background().List()
	if len(list) != 1 {
		t.Fatalf("registry has %d entries, want the foreground child", len(list))
	}
	if list[0].Background {
		t.Error("a foreground child was recorded as background")
	}
	if !list[0].Done() {
		t.Error("the child is still running after Spawn returned")
	}
	if got := sp.Background().PendingReport(); got != "" {
		t.Errorf("the foreground result was also delivered as a background report:\n%s", got)
	}
}
