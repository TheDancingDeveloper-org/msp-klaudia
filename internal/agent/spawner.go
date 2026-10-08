package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/gitprobe"
	"github.com/greenthread-ai/klaudia/internal/hooks"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/subagent"
	"github.com/greenthread-ai/klaudia/internal/tools"
	"github.com/greenthread-ai/klaudia/internal/worktree"
)

// ChildSpec is the launching turn's state, captured at the moment the Agent
// tool fires rather than at process wiring. A child that ran on the wiring-time
// snapshot could never ask the user, never saw a /mode or /model change, and
// its edits were invisible to /undo. The zero value means "nothing was
// captured" and falls back to what the Spawner was built with, which is what
// every caller that predates the per-call capture still does.
type ChildSpec struct {
	// Approver resolves the child's permission asks. Nil falls back to the
	// Spawner's approver.
	Approver Approver
	// Mode is the live permission mode of the launching turn. Nil falls back to
	// the Spawner's permission context.
	Mode func() permission.Mode
	// Model is the launching turn's model id. "" keeps the Spawner's model.
	Model string
	// Effort and Thinking are the launching turn's reasoning settings.
	Effort   string
	Thinking string
	// BeforeEdit is the launching turn's pre-edit checkpoint. The child calls
	// it before its own writes, and adoption calls it with the files it is
	// about to apply.
	BeforeEdit func(tool string, paths []string)
	// ExtraDirs are the session's additional working directories, named in the
	// child's system prompt.
	ExtraDirs []string
	// Budget, when non-nil, bounds the child's own spend in USD. Nil means the
	// launching turn has no budget, so the child is not bounded by one either.
	Budget *float64
	// WorkingDir is the session's working directory. "" keeps the Spawner's.
	WorkingDir string
	// RequestedDir is the Agent tool's working_dir input: the repository the
	// child should be cut from, instead of the session's. "" means the
	// session's. It is validated against WorkingDir and ExtraDirs.
	RequestedDir string
	// Conversation is the launching turn's Turn.Conversation, so the child's
	// registry entry is delivered back to the same conversation. "" for a
	// frontend with only one.
	Conversation string
}

// childSpecFrom reads the launching turn's state off a tools.ParentContext.
// nil — a caller that predates the per-call capture — is the zero spec, which
// falls back to what the Spawner was built with. Anything else that does not
// implement the interface is the zero spec too: a spec the spawner cannot
// read is no better than none. A requested working directory travels on the
// spec (tools.requestedDirSpec) rather than on ParentContext, because it is
// one tool call's input and not a property of the turn.
func childSpecFrom(spec any) ChildSpec {
	requested := ""
	if r, ok := spec.(interface{ RequestedWorkingDir() string }); ok && r != nil {
		requested = r.RequestedWorkingDir()
	}
	pc, ok := spec.(tools.ParentContext)
	if !ok || pc == nil {
		return ChildSpec{RequestedDir: requested}
	}
	approver, _ := pc.ParentApprover().(Approver)
	return ChildSpec{
		Approver:     approver,
		Mode:         pc.ParentMode(),
		Model:        pc.ParentModel(),
		Effort:       pc.ParentEffort(),
		Thinking:     pc.ParentThinking(),
		BeforeEdit:   pc.ParentBeforeEdit(),
		ExtraDirs:    pc.ParentExtraDirs(),
		Budget:       pc.ParentBudget(),
		WorkingDir:   pc.ParentWorkingDir(),
		Conversation: pc.ParentConversation(),
		RequestedDir: requested,
	}
}

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

	// background tracks sub-agents launched with SpawnBackground. Created lazily
	// so a Spawner built without one (older callers, tests) still runs
	// synchronous sub-agents.
	background *BackgroundRegistry

	// guard is the parent's CommandGuard, captured at launch. Background
	// children run on a context detached from the parent turn, so the guard
	// cannot travel on that context the way a synchronous child's does.
	guard func(tool string, input []byte, cwd string) string

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

// WithWorktrees turns per-sub-agent checkouts on or off for every sub-agent
// that can write, synchronous or background ([subagents] worktree in config;
// on by default). Both paths use internal/worktree: a seeded checkout,
// adopted back on success and kept on failure.
func (s *Spawner) WithWorktrees(on bool) *Spawner {
	s.isolate = on
	return s
}

func (s *Spawner) WithHostGate(g *HostGate) *Spawner {
	s.hostGate = g
	return s
}

// WithCommandGuard records the parent's guard for background children. A
// synchronous child inherits it from its context (WithCommandGuard on ctx);
// a background child does not have that context, because its cancellation
// must outlive the turn that started it.
func (s *Spawner) WithCommandGuard(guard func(tool string, input []byte, cwd string) string) *Spawner {
	s.guard = guard
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
func (s *Spawner) Spawn(ctx context.Context, spec any, subagentType, prompt string, progress func(string)) (string, *tools.ChildUsage, error) {
	return s.spawn(ctx, childSpecFrom(spec), subagentType, prompt, progress)
}

// spawn is Spawn with the spec already converted. The exported signature takes
// an any because tools.Spawner cannot name ChildSpec (agent imports tools).
func (s *Spawner) spawn(ctx context.Context, spec ChildSpec, subagentType, prompt string, progress func(string)) (string, *tools.ChildUsage, error) {
	types := s.types
	if len(types) == 0 {
		types = subagent.Builtin()
	}
	t, ok := subagent.Find(types, subagentType)
	if !ok {
		return "", nil, fmt.Errorf("unknown subagent_type %q", subagentType)
	}
	// The child sees the environment and the project's instructions, not only
	// its type's prompt; t is a copy, so the built-in stays as it was.
	t.SystemPrompt = subagentSystem(t.SystemPrompt, s.workingDir, spec.ExtraDirs)

	repo, sub, prov, err := s.childRepo(spec)
	if err != nil {
		return "", nil, err
	}
	dir := repo
	var tree *worktree.Tree
	isolate := s.isolate && t.Isolation != "none" && dir != "" && writesFiles(subagentTools(t.Filter(s.base))) && worktree.Supported(ctx, dir)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reg := s.Background()
	id := reg.register(spec.Conversation, subagentType, "", isolate, prov.String(), false, cancel)

	var childErr error
	defer func() {
		// A foreground child is delivered by the tool result, not by the
		// background poll, so it is marked collected the moment it ends.
		reg.finish(id, "", nil, childErr)
		reg.collected(id)
	}()
	if isolate {
		if wt, err := worktree.New(ctx, dir, subagentType); err != nil {
			reportf(progress, "  (sharing the working tree: %v)", err)
		} else {
			tree, dir = wt, filepath.Join(wt.Dir, sub)
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
	text, usage, err := s.runChild(ctx, spec, t, prompt, dir, emit)
	childErr = err
	if tree == nil {
		return prov.note(text), usage, err
	}
	if err != nil {
		// Whatever the child wrote stays where it is: applying half a change to
		// the user's tree is the one outcome isolation exists to prevent.
		return tree.Rewrite(text), usage, fmt.Errorf("%w (the sub-agent's changes are left in %s)", err, tree.Dir)
	}
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeCleanupTimeout)
	defer cancel()
	return prov.note(s.collect(done, tree, tree.Rewrite(text), spec.BeforeEdit, progress)), usage, nil
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
// final text plus what it spent, applying the shared turn/context bounds. It
// is the single body both the synchronous Spawn and the background goroutine
// drive. usage is nil when the run never started.
func (s *Spawner) runChild(ctx context.Context, spec ChildSpec, t subagent.Type, prompt, workingDir string, emit Emitter) (string, *tools.ChildUsage, error) {
	childTools := subagentTools(t.Filter(s.base))
	model := subagentModel(s.model, t.Model)
	if spec.Model != "" {
		// The launching turn's model wins over the wiring-time one: /model
		// between startup and this launch has to reach the child. A type that
		// names its own model still wins over both.
		model = subagentModel(anthropic.Model(spec.Model), t.Model)
	}

	perm := s.permission
	if spec.Mode != nil {
		perm = permission.Context{Mode: spec.Mode}
	}
	approver := s.approver
	if spec.Approver != nil {
		approver = spec.Approver
	}
	var budget float64
	if spec.Budget != nil {
		if *spec.Budget <= 0 {
			// 0 would mean "no budget" to the loop, which is the opposite of
			// what an exhausted parent means. Refuse rather than launch a child
			// that can spend without limit.
			return "", nil, fmt.Errorf("the session's budget is spent; not launching a sub-agent")
		}
		budget = *spec.Budget
	}

	maxTurns := s.maxTurns
	if t.MaxTurns > 0 {
		// The type's own bound wins over the session's, and is itself capped by
		// it when the session set one, so a file cannot raise the ceiling.
		maxTurns = t.MaxTurns
		if s.maxTurns > 0 && s.maxTurns < maxTurns {
			maxTurns = s.maxTurns
		}
	}
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
		MaxBudgetUSD:  budget,
		Permission:    perm,
		Host:          s.hostGate,
		WorkingDir:    workingDir,
		Approver:      approver,
		BeforeEdit:    spec.BeforeEdit,
		Effort:        spec.Effort,
		Thinking:      spec.Thinking,
		ContextWindow: ctxWindow,
		ProviderName:  s.providerName,
		DeferredTools: filterDeferred(s.deferred(), childTools),
		Hooks:         s.hooks,
		CommandGuard:  s.guard,
		SubAgent:      true,
	}, emit)
	usage := childUsageOf(string(model), res)
	if err != nil {
		// What the sub-agent had already worked out is not lost with it: the
		// last reply it completed goes back with the error, marked as partial.
		// Returning "" threw away everything it found before a stream error
		// or an overload ended its run.
		if res.Text != "" {
			return fmt.Sprintf("[Sub-agent failed after %d turn(s); its last completed reply follows and may be incomplete.]\n\n%s", res.NumTurns, res.Text), usage, err
		}
		return "", usage, err
	}
	// Say so rather than passing back a truncated answer as if it were complete.
	if res.StopReason == "max_turns" || res.StopReason == "max_budget" {
		note := fmt.Sprintf("[Sub-agent stopped at its %d-turn limit before finishing. "+
			"The result below may be incomplete.]", maxTurns)
		if res.StopReason == "max_budget" {
			note = "[Sub-agent stopped because it reached its budget before finishing. " +
				"The result below may be incomplete.]"
		}
		if res.Text == "" {
			return note + subagentUsage(res, time.Since(start)), usage, nil
		}
		return note + "\n\n" + res.Text + subagentUsage(res, time.Since(start)), usage, nil
	}
	return res.Text + subagentUsage(res, time.Since(start)), usage, nil
}

// childUsageOf is the child's spend in the shape the parent folds into its
// own totals. Nil when the child never made a request, so a launch that
// failed before the loop does not add a zero row.
func childUsageOf(model string, res Result) *tools.ChildUsage {
	if res.NumTurns == 0 && res.CostUSD == 0 && res.InputTokens == 0 && res.OutputTokens == 0 {
		return nil
	}
	return &tools.ChildUsage{
		Model:                    model,
		InputTokens:              res.InputTokens,
		OutputTokens:             res.OutputTokens,
		CacheReadInputTokens:     res.CacheReadInputTokens,
		CacheCreationInputTokens: res.CacheCreationInputTokens,
		APIDuration:              res.APIDuration,
		CostUSD:                  res.CostUSD,
		NumTurns:                 res.NumTurns,
	}
}

// SpawnBackground launches a sub-agent that runs independently of the parent
// turn and returns its handle immediately. The result is delivered later, via
// the registry (PendingReport → Options.CollectBackground) and shown live by
// the /agents view. label is the model's task description, for the status view.
// conversation is the launching turn's Turn.Conversation (tools.Context): the
// result is delivered only to that conversation.
//
// A writer (writesFiles on the granted registry) runs in a seeded git checkout
// when [subagents] worktree is on and the project is a repository, so
// concurrent writers cannot corrupt each other's tree; a read-only type shares
// the parent tree. The checkout is the same one a synchronous writer gets
// (internal/worktree): adopted back on success, kept on failure or conflict.
// The child runs on a context detached from the parent turn — it must outlive
// the turn that started it — so its cancellation is tracked in the registry
// (Cancel) rather than tied to ctx.
func (s *Spawner) SpawnBackground(conversation string, spec any, subagentType, prompt, label string, progress func(string)) (string, string, error) {
	return s.spawnBackground(conversation, childSpecFrom(spec), subagentType, prompt, label, progress)
}

func (s *Spawner) spawnBackground(conversation string, spec ChildSpec, subagentType, prompt, label string, progress func(string)) (string, string, error) {
	types := s.types
	if len(types) == 0 {
		types = subagent.Builtin()
	}
	t, ok := subagent.Find(types, subagentType)
	if !ok {
		return "", "", fmt.Errorf("unknown subagent_type %q", subagentType)
	}
	// The child sees the environment and the project's instructions, not only
	// its type's prompt; t is a copy, so the built-in stays as it was.
	t.SystemPrompt = subagentSystem(t.SystemPrompt, s.workingDir, spec.ExtraDirs)

	repo, sub, prov, err := s.childRepo(spec)
	if err != nil {
		return "", "", err
	}
	reg := s.Background()
	isolate := s.isolate && t.Isolation != "none" && repo != "" && writesFiles(subagentTools(t.Filter(s.base))) && worktree.Supported(context.Background(), repo)

	ctx, cancel := context.WithCancel(context.Background())
	id := reg.register(conversation, subagentType, label, isolate, prov.String(), true, cancel)

	// Background progress cannot go to the launching tool call — that returned
	// the moment we handed back the id — so it updates the registry entry, which
	// is what the /agents view reads for a live activity line. The callback the
	// caller passed is honoured too: the frontend that launched the agent still
	// wants the lines.
	emit := progressEmitter(func(line string) {
		reg.setActivity(id, line)
		reportf(progress, "%s", line)
	})

	go func() {
		defer cancel()
		workingDir := repo
		var tree *worktree.Tree
		if isolate {
			wt, err := worktree.New(ctx, repo, subagentType)
			if err != nil {
				// Isolation is a safety property, not a nicety: rather than run a
				// writer in the shared tree and risk corrupting it, fail the agent
				// and say why.
				reg.finish(id, "", nil, fmt.Errorf("could not isolate a worktree: %w", err))
				return
			}
			tree, workingDir = wt, filepath.Join(wt.Dir, sub)
			reportf(progress, "  ↳ isolated checkout %s", wt.Dir)
		}
		result, usage, err := s.runChild(ctx, spec, t, prompt, workingDir, emit)
		if tree == nil {
			reg.finish(id, prov.note(result), usage, err)
			return
		}
		if err != nil {
			// Whatever the child wrote stays where it is: applying half a change
			// to the user's tree is the one outcome isolation exists to prevent.
			reg.finish(id, tree.Rewrite(result), usage, fmt.Errorf("%w (the sub-agent's changes are left in %s)", err, tree.Dir))
			return
		}
		done, stop := context.WithTimeout(context.Background(), worktreeCleanupTimeout)
		defer stop()
		reg.finish(id, prov.note(s.collect(done, tree, tree.Rewrite(result), spec.BeforeEdit, progress)), usage, nil)
	}()

	return id, prov.String(), nil
}

// childRepo resolves the repository a child is cut from. With no requested
// directory that is the session's working directory (the spawner's, when the
// turn did not say). A requested directory must sit inside that working
// directory or one of the session's additional directories, and it is then
// resolved to its repository toplevel: the seed and the adoption both happen
// there, so a path inside a nested checkout still lands in the right tree.
// A directory that is not a repository is used as given — a child can still
// share it — and says so rather than naming a branch it does not have.
// childRepo resolves the repository a child is cut from and the subpath the
// session sits at inside it. The subpath is "" when the session is the
// repository toplevel. The child runs at <checkout>/<sub> and the adoption
// runs from the toplevel, so an edit outside the session's subdirectory is
// still applied instead of being listed and then discarded.
func (s *Spawner) childRepo(spec ChildSpec) (repo, sub string, prov repoProvenance, err error) {
	base := spec.WorkingDir
	if base == "" {
		base = s.workingDir
	}
	if spec.RequestedDir == "" {
		repo, prov = base, repoOf(base)
	} else {
		dir := canonical(spec.RequestedDir)
		if !dirAllowed(dir, append([]string{base}, spec.ExtraDirs...)) {
			return "", "", repoProvenance{}, fmt.Errorf("working_dir %q is outside the session's working directory and its additional directories", spec.RequestedDir)
		}
		top := repoToplevel(dir)
		if top == "" {
			return dir, "", repoProvenance{Repo: dir}, nil
		}
		repo, prov = top, repoOf(top)
	}
	if top := repoToplevel(repo); top != "" && top != canonical(repo) {
		rel, relErr := filepath.Rel(top, canonical(repo))
		if relErr == nil && rel != "." {
			sub = rel
		}
		repo, prov = top, repoOf(top)
	}
	return repo, sub, prov, nil
}

// dirAllowed reports whether dir is inside one of roots. Both sides are
// canonicalised first, so a symlink or a ".." cannot step outside a root
// that was named by its real path. A root that cannot be canonicalised is
// skipped rather than treated as a match.
func dirAllowed(dir string, roots []string) bool {
	dir = canonical(dir)
	for _, root := range roots {
		if root == "" {
			continue
		}
		root = canonical(root)
		if dir == root {
			return true
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// canonical is the real, absolute path, falling back to the cleaned absolute
// path when the directory does not exist yet.
func canonical(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return filepath.Clean(dir)
}

// repoProvenance is where a child was cut from: the repository toplevel, the
// branch and the HEAD commit. Zero when the directory is not a repository.
type repoProvenance struct {
	Repo   string
	Branch string
	Head   string
}

// note prepends the provenance to a child's result, so the launch result
// names the repo, branch and HEAD the child was cut from. A result with no
// provenance — the session directory is not a repository — is unchanged.
func (p repoProvenance) note(text string) string {
	if p.Repo == "" {
		return text
	}
	line := "Cut from " + p.Repo
	if p.Branch != "" {
		line += " on " + p.Branch
	}
	if p.Head != "" {
		line += " at " + p.Head
	}
	return "[" + line + "]\n\n" + text
}

// String is the one-line form the /agents view prints. A zero provenance
// prints nothing, so a child cut from a plain directory adds no line.
func (p repoProvenance) String() string {
	if p.Repo == "" {
		return ""
	}
	line := p.Repo
	if p.Branch != "" {
		line += " on " + p.Branch
	}
	if p.Head != "" {
		line += " at " + p.Head
	}
	return line
}

// repoOf reads a directory's toplevel, branch and short HEAD. A directory
// that is not a repository, or a git that fails, gives the zero value: the
// child still runs, it just cannot name where it was cut from.
func repoOf(dir string) repoProvenance {
	top := repoToplevel(dir)
	if top == "" {
		return repoProvenance{}
	}
	return repoProvenance{Repo: top, Branch: gitOut(top, "rev-parse", "--abbrev-ref", "HEAD"), Head: gitOut(top, "rev-parse", "--short", "HEAD")}
}

// repoToplevel resolves dir to its repository toplevel, or "" when dir is
// not inside one.
func repoToplevel(dir string) string {
	return gitOut(dir, "rev-parse", "--show-toplevel")
}

// gitOut runs one guarded read-only git command and returns its trimmed
// stdout, or "" when git is absent or the command fails.
func gitOut(dir string, args ...string) string {
	cmd := gitprobe.Command(dir, args...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
func (s *Spawner) collect(ctx context.Context, tree *worktree.Tree, text string, beforeEdit func(string, []string), progress func(string)) string {
	rep, err := tree.Adopt(ctx)
	if err != nil {
		reportf(progress, "  (could not apply the sub-agent's changes: %v)", err)
		return text + fmt.Sprintf("\n\n[The sub-agent's file changes could not be applied to the "+
			"working tree (%v). They are in %s.]", err, tree.Dir)
	}
	if beforeEdit != nil && len(rep.Adopted) > 0 {
		// Adoption applies the child's diff in one shot, so there is no moment
		// before each file to snapshot — but the files still have to be reported,
		// or a child's edits stay invisible to /undo, /changes and /commit.
		beforeEdit("Agent", rep.Adopted)
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
