package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/hooks"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/subagent"
	"github.com/greenthread-ai/klaudia/internal/tools"
	"github.com/greenthread-ai/klaudia/internal/worktree"
)

// Spawner runs sub-agents. It implements tools.Spawner so the Agent tool can
// launch a child loop with a filtered toolset and the type's system prompt.
type Spawner struct {
	provider   api.Provider
	base       *tools.Registry
	model      anthropic.Model
	types      []subagent.Type // selectable types; nil means the built-ins
	permission permission.Context
	approver   Approver
	maxTurns   int

	// mu guards deferredTools, which an MCP config reload replaces while the
	// agent loop is reading it to spawn a sub-agent. The map is swapped
	// wholesale and never mutated in place, so a reader that has taken a
	// reference may safely use it after releasing the lock.
	mu            sync.RWMutex
	deferredTools map[string]bool

	workingDir   string
	hostGate     *HostGate
	providerName string

	// background tracks sub-agents launched with SpawnBackground; worktrees
	// isolates the writers among them. Both are set lazily so a Spawner built
	// without them (older callers, tests) still runs synchronous sub-agents.
	background *BackgroundRegistry
	worktrees  WorktreeProvider

	// hooks runs the user's lifecycle hooks in every child, so a formatter
	// that runs on each edit also runs on a sub-agent's edits.
	hooks *hooks.Runner
	// isolate gives a synchronous sub-agent that can write its own seeded git
	// checkout (internal/worktree), adopted back into the user's tree when it
	// finishes. Upstream's design; see docs/working-tree.md.
	isolate bool
}

// Background returns the registry of background sub-agents this Spawner has
// launched, creating it on first use. The frontend lists it (/agents) and the
// loop collects finished results from it (PendingReport).
func (s *Spawner) Background() *BackgroundRegistry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.background == nil {
		s.background = NewBackgroundRegistry()
	}
	return s.background
}

// WithWorktrees sets the provider that isolates background writers. Passing nil
// disables isolation (writers then share the parent tree — acceptable for a
// single writer, unsafe for concurrent ones). The default wiring supplies a
// git-backed provider; tests inject a fake.
func (s *Spawner) WithBackgroundWorktrees(w WorktreeProvider) *Spawner {
	s.worktrees = w
	return s
}

// SetDeferred replaces the deferred-tool set. A config reload can add or drop
// MCP tools mid-session, and a sub-agent spawned afterwards should inherit the
// set as it is now, not as it was at startup.
func (s *Spawner) SetDeferred(deferred map[string]bool) {
	next := make(map[string]bool, len(deferred))
	for name, ok := range deferred {
		next[name] = ok
	}
	s.mu.Lock()
	s.deferredTools = next
	s.mu.Unlock()
}

// deferred returns the current deferred-tool set.
func (s *Spawner) deferred() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deferredTools
}

// WithWorkingDir sets the project root sub-agents inherit. Without it a
// sub-agent's tools would run in the process cwd while the parent's run in the
// project — the kind of split that makes path-based policy meaningless.
func (s *Spawner) WithWorkingDir(dir string) *Spawner {
	s.workingDir = dir
	return s
}

// WithProviderName records the configured provider ("" = anthropic), so a
// sub-agent sizes its context window and output cap the way the parent does:
// from Claude's tables on Anthropic, from the unknown-model defaults elsewhere.
func (s *Spawner) WithProviderName(name string) *Spawner {
	s.providerName = name
	return s
}

// WithHostGate gives sub-agents the parent's trust gate.
//
// Sharing the gate — and therefore the ledger — is the point: a sub-agent must
// not be a way around the boundary, and an approval the user gave the parent
// should cover the child doing the work. Without this a sub-agent's Bash calls
// would be unclassified, which is the easiest hole to leave and the hardest to
// notice.
// WithHooks runs h in every child loop.
func (s *Spawner) WithHooks(h *hooks.Runner) *Spawner {
	s.hooks = h
	return s
}

// WithWorktrees turns per-sub-agent checkouts on or off for synchronous
// sub-agents that can write ([subagents] worktree in config; on by default).
// Background writers are isolated separately (WithBackgroundWorktrees).
func (s *Spawner) WithWorktrees(on bool) *Spawner {
	s.isolate = on
	return s
}

func (s *Spawner) WithHostGate(g *HostGate) *Spawner {
	s.hostGate = g
	return s
}

// NewSpawner builds a Spawner. base is the registry sub-agents draw tools from
// (typically the local tools without the Agent tool itself, to bound recursion).
// approver resolves permission asks for sub-agents (inherited from the parent
// frontend); nil falls back to DenyAll.
func NewSpawner(provider api.Provider, base *tools.Registry, model anthropic.Model, perm permission.Context, approver Approver, maxTurns int) *Spawner {
	return NewSpawnerWithDeferred(provider, base, model, perm, approver, maxTurns, nil)
}

// NewSpawnerWithDeferred builds a Spawner that also respects the parent's
// deferred tool map after applying the sub-agent type allowlist.
func NewSpawnerWithDeferred(provider api.Provider, base *tools.Registry, model anthropic.Model, perm permission.Context, approver Approver, maxTurns int, deferred map[string]bool) *Spawner {
	copyDeferred := make(map[string]bool, len(deferred))
	for name, ok := range deferred {
		copyDeferred[name] = ok
	}
	return &Spawner{provider: provider, base: base, model: model, permission: perm, approver: approver, maxTurns: maxTurns, deferredTools: copyDeferred}
}

// defaultSubagentMaxTurns bounds a sub-agent when the CLI sets no explicit
// --max-turns (its default is 0, "unlimited"). Unlimited is a defensible
// default for the main loop, where the user watches each step and can interrupt;
// for a child it means an invisible loop that can run until someone notices.
// Generous enough for real research, finite enough to end.
const defaultSubagentMaxTurns = 50

// Spawn runs a sub-agent of the named type to completion and returns its final
// text. It satisfies tools.Spawner.
//
// progress, when non-nil, receives a short line per child tool call. Passing
// nil (as headless callers do) restores the previous silent behaviour.
func (s *Spawner) Spawn(ctx context.Context, subagentType, prompt string, progress func(string)) (string, error) {
	types := s.types
	if len(types) == 0 {
		types = subagent.Builtin()
	}
	t, ok := subagent.Find(types, subagentType)
	if !ok {
		return "", fmt.Errorf("unknown subagent_type %q", subagentType)
	}
	// The child sees the environment and the project's instructions, not only
	// its type's prompt; t is a copy, so the built-in stays as it was.
	t.SystemPrompt = subagentSystem(t.SystemPrompt, s.workingDir)

	// A child that can write gets a checkout of its own, seeded with the
	// user's uncommitted work and adopted back when it finishes: two writers in
	// one tree both succeed at producing a mess. A failure to create it falls
	// back to sharing the tree, and says so.
	dir := s.workingDir
	var tree *worktree.Tree
	if s.isolate && dir != "" && writesFiles(subagentTools(t.Filter(s.base))) && worktree.Supported(ctx, dir) {
		if wt, err := worktree.New(ctx, dir, subagentType); err != nil {
			reportf(progress, "  (sharing the working tree: %v)", err)
		} else {
			tree, dir = wt, wt.Dir
			reportf(progress, "  ↳ isolated checkout %s", wt.Dir)
		}
	}

	// Relay the child's activity upward. Without an emitter the child ran
	// completely dark: the frontend saw one Agent tool call and nothing until it
	// returned, so a twenty-minute research run and a hang looked identical.
	// Paths in the child's own checkout are paths the user cannot open, so they
	// are rewritten to where the file really is.
	var emit Emitter
	if progress != nil {
		emit = progressEmitter(func(line string) { progress(tree.Rewrite(line)) })
	}
	text, err := s.runChild(ctx, t, prompt, dir, emit)
	if tree == nil {
		return text, err
	}
	if err != nil {
		// Whatever the child wrote stays where it is: applying half a change to
		// the user's tree is the one outcome isolation exists to prevent.
		return tree.Rewrite(text), fmt.Errorf("%w (the sub-agent's changes are left in %s)", err, tree.Dir)
	}
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeCleanupTimeout)
	defer cancel()
	return s.collect(done, tree, tree.Rewrite(text), progress), nil
}

// progressEmitter adapts a per-line progress callback into a child Emitter.
// Passing nil returns nil, restoring the previous silent behaviour.
func progressEmitter(progress func(string)) Emitter {
	if progress == nil {
		return nil
	}
	return func(ev Event) {
		if line := subagentProgressLine(ev); line != "" {
			progress(line)
		}
	}
}

// runChild runs one sub-agent loop to completion in workingDir and returns its
// final text, applying the shared turn/context bounds. It is the single body
// both the synchronous Spawn and the background goroutine drive.
func (s *Spawner) runChild(ctx context.Context, t subagent.Type, prompt, workingDir string, emit Emitter) (string, error) {
	childTools := subagentTools(t.Filter(s.base))
	model := subagentModel(s.model, t.Model)

	maxTurns := s.maxTurns
	if maxTurns <= 0 {
		maxTurns = defaultSubagentMaxTurns
	}
	// Give the child the model's real window. Leaving this 0 fell back to the
	// 200k compaction default, so a sub-agent on a 1M model summarised its
	// history at a fifth of the room it actually had.
	ctxWindow, _ := api.ContextWindowFor(s.providerName, string(model), 0)

	start := time.Now()
	loop := New(s.provider, childTools)
	res, err := loop.Run(ctx, Options{
		Prompt:        prompt,
		Model:         model,
		System:        t.SystemPrompt,
		MaxTurns:      maxTurns,
		Permission:    s.permission,
		Host:          s.hostGate,
		WorkingDir:    workingDir,
		Approver:      s.approver,
		ContextWindow: ctxWindow,
		ProviderName:  s.providerName,
		DeferredTools: filterDeferred(s.deferred(), childTools),
		Hooks:         s.hooks,
		SubAgent:      true,
	}, emit)
	if err != nil {
		// What the sub-agent had already worked out is not lost with it: the
		// last reply it completed goes back with the error, marked as partial.
		// Returning "" threw away everything it found before a stream error
		// or an overload ended its run.
		if res.Text != "" {
			return fmt.Sprintf("[Sub-agent failed after %d turn(s); its last completed reply follows and may be incomplete.]\n\n%s", res.NumTurns, res.Text), err
		}
		return "", err
	}
	// Say so rather than passing back a truncated answer as if it were complete.
	if res.StopReason == "max_turns" {
		note := fmt.Sprintf("[Sub-agent stopped at its %d-turn limit before finishing. "+
			"The result below may be incomplete.]", maxTurns)
		if res.Text == "" {
			return note + subagentUsage(res, time.Since(start)), nil
		}
		return note + "\n\n" + res.Text + subagentUsage(res, time.Since(start)), nil
	}
	return res.Text + subagentUsage(res, time.Since(start)), nil
}

// SpawnBackground launches a sub-agent that runs independently of the parent
// turn and returns its handle immediately. The result is delivered later, via
// the registry (PendingReport → Options.CollectBackground) and shown live by
// the /agents view. label is the model's task description, for the status view.
// conversation is the launching turn's Turn.Conversation (tools.Context): the
// result is delivered only to that conversation.
//
// A writer type (subagent.Type.MayWrite) runs in an isolated git worktree when
// a provider is configured, so concurrent writers cannot corrupt each other's
// tree; a read-only type shares the parent tree. The child runs on a context
// detached from the parent turn — it must outlive the turn that started it — so
// its cancellation is tracked in the registry (Cancel) rather than tied to ctx.
func (s *Spawner) SpawnBackground(conversation, subagentType, prompt, label string, progress func(string)) (string, error) {
	t, ok := subagent.Lookup(subagentType)
	if !ok {
		return "", fmt.Errorf("unknown subagent_type %q", subagentType)
	}

	reg := s.Background()
	isolate := t.MayWrite() && s.worktrees != nil && s.workingDir != ""

	ctx, cancel := context.WithCancel(context.Background())
	id := reg.register(conversation, subagentType, label, isolate, cancel)

	// Background progress cannot go to the launching tool call — that returned
	// the moment we handed back the id — so it updates the registry entry, which
	// is what the /agents view reads for a live activity line.
	emit := progressEmitter(func(line string) { reg.setActivity(id, line) })

	go func() {
		defer cancel()
		workingDir := s.workingDir
		var cleanup func() error
		if isolate {
			dir, cl, err := s.worktrees.Create(ctx, s.workingDir, id)
			if err != nil {
				// Isolation is a safety property, not a nicety: rather than run a
				// writer in the shared tree and risk corrupting it, fail the agent
				// and say why.
				reg.finish(id, "", fmt.Errorf("could not isolate a worktree: %w", err))
				return
			}
			workingDir, cleanup = dir, cl
		}
		if cleanup != nil {
			defer func() { _ = cleanup() }()
		}
		result, err := s.runChild(ctx, t, prompt, workingDir, emit)
		reg.finish(id, result, err)
	}()

	return id, nil
}

// worktreeCleanupTimeout bounds adoption and removal. Both are a handful of git
// commands on a tree that is already on disk; a limit this generous only ever
// fires on a repository that has gone wrong, and the alternative is a tool call
// that never returns.
const worktreeCleanupTimeout = 2 * time.Minute

// collect applies the child's work to the user's tree and tells the model what
// landed.
//
// The model is told because it has to be: it asked a child to change files, and
// "b.txt was not applied" changes what it should do next. A silent conflict
// would have it carry on describing work that is not in the tree.
func (s *Spawner) collect(ctx context.Context, tree *worktree.Tree, text string, progress func(string)) string {
	rep, err := tree.Adopt(ctx)
	if err != nil {
		reportf(progress, "  (could not apply the sub-agent's changes: %v)", err)
		return text + fmt.Sprintf("\n\n[The sub-agent's file changes could not be applied to the "+
			"working tree (%v). They are in %s.]", err, tree.Dir)
	}
	if len(rep.Conflicted) > 0 {
		// The conflicting version exists only in the checkout, so removing it
		// would destroy the only copy of work the user may want.
		reportf(progress, "  ↳ %s", rep.Summary())
		return text + fmt.Sprintf("\n\n[Working tree: %s. The sub-agent's versions of those files "+
			"are in %s.]", rep.Summary(), tree.Dir)
	}
	if err := tree.Remove(ctx); err != nil {
		reportf(progress, "  (left the sub-agent's checkout at %s: %v)", tree.Dir, err)
	}
	if rep.Empty() {
		return text
	}
	reportf(progress, "  ↳ %s", rep.Summary())
	return text + fmt.Sprintf("\n\n[Working tree: %s.]", rep.Summary())
}

// writesFiles reports whether this toolset can change the working tree.
//
// Four names rather than a capability on the Tool interface: these are the
// local tools that touch files, and the list is short and stable. MCP tools are
// deliberately not counted even when they are not read-only — their writes land
// on a server, which a second checkout does not isolate, and counting them
// would hand a research agent a pointless copy of the repository.
func writesFiles(r *tools.Registry) bool {
	for _, name := range []string{"Write", "Edit", "NotebookEdit", "Bash"} {
		if _, ok := r.Lookup(name); ok {
			return true
		}
	}
	return false
}

// reportf sends one formatted progress line, if anyone is listening.
func reportf(progress func(string), format string, args ...any) {
	if progress == nil {
		return
	}
	progress(fmt.Sprintf(format, args...))
}

func filterDeferred(deferred map[string]bool, registry *tools.Registry) map[string]bool {
	if len(deferred) == 0 {
		return nil
	}
	out := map[string]bool{}
	for name, ok := range deferred {
		if !ok {
			continue
		}
		if _, exists := registry.Lookup(name); exists {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WithTypes sets the sub-agent types Spawn can run (see subagent.Load).
func (s *Spawner) WithTypes(types []subagent.Type) *Spawner {
	s.types = types
	return s
}

// subagentModel is the model a sub-agent runs on: its own when it names one,
// else the parent's. Agent files written for Claude Code say "sonnet" or
// "opus"; on an OpenAI-compatible endpoint those resolve to Claude ids the
// endpoint does not serve, so a non-Claude parent keeps its own model unless
// the agent names a non-Claude one.
func subagentModel(parent anthropic.Model, own string) anthropic.Model {
	if strings.TrimSpace(own) == "" {
		return parent
	}
	m := api.ResolveModel(own)
	if !strings.HasPrefix(string(parent), "claude-") && strings.HasPrefix(string(m), "claude-") {
		return parent
	}
	return m
}
