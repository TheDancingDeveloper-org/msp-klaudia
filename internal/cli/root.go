package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/greenthread-ai/klaudia/internal/acp"
	"github.com/greenthread-ai/klaudia/internal/agent"
	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/browser"
	"github.com/greenthread-ai/klaudia/internal/config"
	"github.com/greenthread-ai/klaudia/internal/doctor"
	"github.com/greenthread-ai/klaudia/internal/gitguard"
	"github.com/greenthread-ai/klaudia/internal/gitprobe"
	"github.com/greenthread-ai/klaudia/internal/goal"
	"github.com/greenthread-ai/klaudia/internal/hooks"
	"github.com/greenthread-ai/klaudia/internal/lsp"
	"github.com/greenthread-ai/klaudia/internal/mcp"
	"github.com/greenthread-ai/klaudia/internal/memory"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/prompt"
	"github.com/greenthread-ai/klaudia/internal/sandbox"
	"github.com/greenthread-ai/klaudia/internal/session"
	"github.com/greenthread-ai/klaudia/internal/skill"
	"github.com/greenthread-ai/klaudia/internal/streamjson"
	"github.com/greenthread-ai/klaudia/internal/subagent"
	"github.com/greenthread-ai/klaudia/internal/tools"
	"github.com/greenthread-ai/klaudia/internal/trust"
	"github.com/greenthread-ai/klaudia/internal/tui"
	"github.com/greenthread-ai/klaudia/internal/version"
)

func compactAndPersist(ctx context.Context, history []anthropic.BetaMessageParam, focus string, compact tui.CompactFunc, onSummary func(string)) ([]anthropic.BetaMessageParam, string, error) {
	newHistory, summary, err := compact(ctx, history, focus)
	if err == nil && onSummary != nil {
		onSummary(summary)
	}
	return newHistory, summary, err
}

// agentWiring is the registry the main loop dispatches from, plus the parts a
// config reload needs in order to rebuild it: the Agent tool to re-append, and
// the spawner whose deferred-tool set has to track the new tool list.
type agentWiring struct {
	registry  *tools.Registry
	spawner   *agent.Spawner
	agentTool tools.Tool
}

// withAgentTool returns a registry that is the base tools plus the Agent tool,
// wired to a sub-agent spawner that draws from the base tools.
//
// The spawner no longer carries the session's approver, model or permission
// mode. Those are the launching turn's, and they change after wiring (/model,
// /mode, a frontend's own approver), so the Agent tool captures them per call
// from tools.Context. What is fixed for the process — the provider, the tool
// registry, the working dir, the trust gate — still lives here.
func withAgentTool(base *tools.Registry, provider api.Provider, maxTurns int, deferred map[string]bool, workingDir string, host *agent.HostGate, types []subagent.Type, worktrees bool) (*agentWiring, error) {
	if len(types) == 0 {
		types = subagent.Builtin()
	}
	spawner := agent.NewSpawnerWithDeferred(provider, base, "", permission.Context{}, nil, maxTurns, deferred).
		WithWorkingDir(workingDir).
		WithHostGate(host).
		WithTypes(types).
		// A sub-agent that can write gets its own seeded checkout, adopted back
		// when it finishes ([subagents] worktree, on by default). One knob for
		// both the synchronous and the background path.
		WithWorktrees(worktrees)

	infos := make([]tools.AgentTypeInfo, 0)
	for _, t := range types {
		infos = append(infos, tools.AgentTypeInfo{Name: t.Name, Description: t.Description})
	}
	agentTool, err := tools.NewAgent(spawner, infos)
	if err != nil {
		return nil, err
	}
	return &agentWiring{
		registry:  tools.NewRegistry(append(base.All(), agentTool)...),
		spawner:   spawner,
		agentTool: agentTool,
	}, nil
}

// skillToolInfos adapts loaded skills into the tools package's SkillInfo,
// binding each skill's Render so the tools package stays decoupled from skill.
func skillToolInfos(skills []skill.Skill) []tools.SkillInfo {
	infos := make([]tools.SkillInfo, 0, len(skills))
	for _, sk := range skills {
		sk := sk
		infos = append(infos, tools.SkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
			ArgHint:     sk.ArgHint,
			Render:      sk.Render,
		})
	}
	return infos
}

// builtinSlashCommands are the TUI commands a skill cannot override (the slash
// switch handles them before the /<skill> default branch).
var builtinSlashCommands = map[string]bool{
	"help": true, "?": true, "quit": true, "exit": true, "clear": true,
	"model": true, "effort": true, "mode": true, "goal": true,
	"memory": true, "mcp": true, "stats": true,
	"status": true,
	"config": true, "agents": true, "context": true,
	"compact": true, "add-dir": true,
	"plan": true, "doctor": true, "diff": true, "commit": true, "export": true,
	"last": true, "resume": true, "rename": true,
}

// withExtraDirs appends an "additional working directories" note to the system
// prompt when /add-dir has registered any (v1: informational context only).
func withExtraDirs(sys string, dirs []string) string {
	if len(dirs) == 0 {
		return sys
	}
	return sys + "\n\nAdditional working directories the user has made available:\n- " +
		strings.Join(dirs, "\n- ")
}

// sessionRoots builds the trust roots for a session: the working directory plus
// any extra in-scope directories (--add-dir at launch, /add-dir at runtime).
// trust.NewRoots canonicalises them and drops any that are unsafe to treat as
// project (a system prefix, or "/"), so an extra dir counts as project work
// rather than a host change on the next tool call.
func sessionRoots(home, cwd string, extra []string) trust.Roots {
	return trust.NewRoots(home, append([]string{cwd}, extra...)...)
}

// buildDoctorInput gathers the facts /doctor reports, without prompting.
//
// hookRunner may be nil — the common case, meaning no hook is configured
// anywhere; doctor is told "none" rather than being left silent about a
// subsystem that runs shell commands when it is on.
func buildDoctorInput(cfg config.Config, model anthropic.Model, cwd, root string, mcpCfg mcp.Config, hookRunner *hooks.Runner) doctor.Input {
	servers, hints := lsp.Survey(cwd)
	// Loaded silently: skill.Load's warnings go to the session that owns the
	// prompt, not to /doctor, which must stay quiet on stderr.
	doctorSkills := make([]doctor.Skill, 0)
	for _, sk := range skill.LoadProject(root, cwd, func(string) {}) {
		scope := "user"
		switch {
		case sk.Bundled:
			scope = "bundled"
		case strings.HasPrefix(sk.Path, root) || strings.HasPrefix(sk.Path, cwd):
			scope = "project"
		}
		doctorSkills = append(doctorSkills, doctor.Skill{Name: sk.Name, Scope: scope})
	}
	lspServers := make([]doctor.LSPServer, 0, len(servers))
	for _, s := range servers {
		lspServers = append(lspServers, doctor.LSPServer{Name: s.Bin, Language: s.Language, Version: s.Version})
	}
	ctxLimit, ctxSource := api.ContextWindowFor(cfg.Provider, string(model), cfg.ContextWindow)
	in := doctor.Input{
		Provider:        providerName(cfg),
		Model:           string(model),
		SandboxMode:     sandboxMode(cfg.Sandbox),
		ConfigFound:     configFileExists(cwd),
		MCPServers:      len(mcpCfg.MCPServers),
		MCPLegacySSE:    legacySSEServers(mcpCfg),
		LSPServers:      lspServers,
		Skills:          doctorSkills,
		Hooks:           doctorHooks(hookRunner),
		MissingLSPHints: hints,
		AuthKind:        "none",
		ContextWindow:   ctxLimit,
		ContextSource:   ctxSource,
		Build:           version.Get().Summary(),
	}
	if cfg.Provider == config.ProviderOpenAI {
		if cfg.ResolveAPIKey() != "" {
			in.AuthOK, in.AuthKind = true, "api-key"
		}
	} else if cred, err := api.ResolveCredential(); err == nil {
		in.AuthOK = true
		if cred.IsOAuth() {
			in.AuthKind = "oauth"
		} else {
			in.AuthKind = "api-key"
		}
	}
	return in
}

// doctorHooks flattens the session's hooks for the report.
func doctorHooks(r *hooks.Runner) []doctor.Hook {
	all := r.All()
	out := make([]doctor.Hook, 0, len(all))
	approved := r.ProjectApproved()
	for _, h := range all {
		scope := "user"
		if h.Project {
			scope = "project"
		}
		out = append(out, doctor.Hook{
			Event:   string(h.Event),
			Matcher: h.Matcher(),
			Scope:   scope,
			Dormant: h.Project && !approved,
		})
	}
	return out
}

// legacySSEServers names the configured MCP servers still pinned to the
// deprecated HTTP+SSE transport, sorted so /doctor reads the same every run.
func legacySSEServers(cfg mcp.Config) []string {
	var out []string
	for name, sc := range cfg.MCPServers {
		if strings.EqualFold(strings.TrimSpace(sc.Type), "sse") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// configFileExists reports whether the global config (config.GlobalPath) or
// the project .klaudia/config.toml exists.
func configFileExists(cwd string) bool {
	if p := config.GlobalPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	_, err := os.Stat(config.ProjectPath(cwd))
	return err == nil
}

// providerName returns the resolved provider for display ("anthropic" default).
func providerName(cfg config.Config) string {
	if cfg.Provider != "" {
		return cfg.Provider
	}
	return config.ProviderAnthropic
}

// sandboxMode returns the resolved sandbox mode for display ("local" default).
func sandboxMode(sb config.Sandbox) string {
	if sb.Mode != "" {
		return sb.Mode
	}
	return config.SandboxLocal
}

// themeOrWarn passes a configured theme through, warning (and falling back to
// the default) when it names an unknown theme so a typo isn't silent.
func themeOrWarn(theme string, warn func(string)) string {
	if theme == "" || tui.IsTheme(theme) {
		return theme
	}
	warn(fmt.Sprintf("unknown theme %q in config; using default (valid: %s)", theme, tui.ThemeNames()))
	return ""
}

// tuiAgents adapts the built-in sub-agent types into the TUI's AgentInfo for
// the /agents command.
func tuiAgents(bs []subagent.Type) []tui.AgentInfo {
	out := make([]tui.AgentInfo, 0, len(bs))
	for _, t := range bs {
		out = append(out, tui.AgentInfo{Name: t.Name, Description: t.Description})
	}
	return out
}

// tuiSkills adapts loaded skills into TUI /<name> commands, warning when a skill
// name shadows a built-in command (the built-in wins, so the skill is
// unreachable as a slash command but remains callable via the Skill tool).
func tuiSkills(skills []skill.Skill, warn func(string)) []tui.SkillCommand {
	out := make([]tui.SkillCommand, 0, len(skills))
	for _, sk := range skills {
		sk := sk
		if tui.IsBuiltinCommand(sk.Name) {
			warn(fmt.Sprintf("skill %q shadows built-in /%s; reachable only via the Skill tool", sk.Name, sk.Name))
		}
		out = append(out, tui.SkillCommand{Name: sk.Name, Description: sk.Description, Render: sk.Render})
	}
	return out
}

// mcpPromptCommands exposes each connected server's MCP prompts (prompts/list)
// as a /mcp__<server>__<prompt> slash command, mirroring the tool namespacing.
// The command's Render fetches the prompt (prompts/get) and returns its rendered
// text, which the TUI submits as the turn prompt; a fetch error is returned as
// text so the failure is visible rather than silent.
//
// Prompts are enumerated once, at startup. A prompt added by a later config
// reload is not yet re-registered as a slash command (the underlying prompt is
// still reachable via the MCP layer) — that UI refresh is tracked as follow-up.
func mcpPromptCommands(ctx context.Context, mgr *mcp.Manager) []tui.SkillCommand {
	prompts := mgr.Prompts(ctx)
	out := make([]tui.SkillCommand, 0, len(prompts))
	for _, p := range prompts {
		p := p
		desc := p.Description
		if desc == "" {
			desc = p.Title
		}
		out = append(out, tui.SkillCommand{
			Name:        p.Qualified,
			Description: strings.TrimSpace("MCP prompt (" + p.Server + "): " + desc),
			Render: func(arguments string) string {
				text, err := mgr.GetPrompt(ctx, p.Server, p.Name, mcp.ParsePromptArgs(p, arguments))
				if err != nil {
					return fmt.Sprintf("MCP prompt %s failed: %v", p.Qualified, err)
				}
				return text
			},
		})
	}
	return out
}

// acpCommands adapts loaded skills into the /commands an ACP client offers in
// its command picker.
//
// Skills only, and no shadowing check: ACP has no built-in commands of its own
// to collide with, because Klaudia's own slash commands live in the TUI and
// cannot run from here (see internal/acp/commands.go).
//
// The hint is set only for skills that actually interpolate $ARGUMENTS. A client
// renders it as placeholder text, so offering "arguments" for a skill that takes
// none invites the user to type something the skill would then append to its own
// body as a stray trailing line.
func acpCommands(skills []skill.Skill) []acp.Command {
	out := make([]acp.Command, 0, len(skills))
	for _, sk := range skills {
		sk := sk
		c := acp.Command{Name: sk.Name, Description: sk.Description, Render: sk.Render}
		if strings.Contains(sk.Body, "$ARGUMENTS") {
			c.Hint = "arguments"
		}
		out = append(out, c)
	}
	return out
}

// acpTranscript opens one transcript per ACP session.
//
// Best effort, matching every other transcript call site: a record that cannot
// be opened is reported to the client's log and the session runs unrecorded,
// because losing the history is not a reason to refuse the work.
func acpTranscript(cwd string, mode permission.Mode) func(string) acp.Transcript {
	return func(sessionID string) acp.Transcript {
		tr, err := session.NewTranscript(session.Meta{
			SessionID:      sessionID,
			CWD:            cwd,
			Version:        version.Version,
			GitBranch:      gitBranch(cwd),
			PermissionMode: string(mode),
		})
		if err != nil {
			return nil
		}
		return tr
	}
}

// acpLoadHistory reads a persisted session back for session/load.
//
// The full transcript, not the compacted summary --resume prefers: the client is
// reopening a conversation to *show* it, and a summary would replay four
// paragraphs of prose in place of the thread the user is looking for. The token
// cost of the longer history is the same cost --resume --full pays.
func acpLoadHistory(cwd string) func(string) ([]anthropic.BetaMessageParam, error) {
	return func(sessionID string) ([]anthropic.BetaMessageParam, error) {
		entries, err := session.Read(session.ExistingPath(cwd, sessionID))
		if err != nil {
			return nil, fmt.Errorf("no session %s in this project: %w", sessionID, err)
		}
		return agent.MessagesFromEntries(entries)
	}
}

// acpSessions lists the project's persisted sessions for session/list.
func acpSessions(cwd string) []acp.SessionSummary {
	all := session.ListProject(cwd)
	out := make([]acp.SessionSummary, 0, len(all))
	for _, s := range all {
		out = append(out, acp.SessionSummary{
			ID:        s.ID,
			Title:     s.Title,
			UpdatedAt: s.Modified.UTC().Format(time.RFC3339),
		})
	}
	return out
}

// modelLister exposes the provider's model enumeration to the TUI when it has
// one. Both shipped providers do, but the capability is an optional interface
// rather than part of Provider — so a future backend that can't list models
// still satisfies Provider, and /model degrades to accepting a typed id.
func modelLister(p api.Provider) func(context.Context) ([]api.ModelInfo, error) {
	lister, ok := p.(api.ModelLister)
	if !ok {
		return nil
	}
	return lister.ListModels
}

// buildProvider selects and constructs the model provider from config. It
// returns the provider and the provider's default model. Anthropic is the
// default; "openai" uses an OpenAI-compatible Chat Completions endpoint.
func buildProvider(cfg config.Config, sessionID string) (api.Provider, string, error) {
	switch cfg.Provider {
	case config.ProviderOpenAI:
		if cfg.BaseURL == "" {
			return nil, "", fmt.Errorf("provider \"openai\" requires baseURL in ~/.klaudia/config.toml or ./.klaudia/config.toml (try --create-config=global or --create-config=local)")
		}
		key := cfg.ResolveAPIKey()
		extraHeaders, missing := cfg.ResolveExtraHeaders()
		if len(missing) > 0 {
			return nil, "", fmt.Errorf("provider \"openai\": extraHeadersEnv references unset environment variable(s): %s — export them before running", strings.Join(missing, ", "))
		}
		if key == "" && len(cfg.ExtraHeadersEnv) == 0 {
			if cfg.APIKey == "" && cfg.APIKeyEnv != "" {
				return nil, "", fmt.Errorf("provider \"openai\" needs apiKey or apiKeyEnv: apiKeyEnv names $%s, which is unset or empty — export it before running", cfg.APIKeyEnv)
			}
			return nil, "", fmt.Errorf("provider \"openai\" needs apiKey or apiKeyEnv (or extraHeadersEnv for a header-authenticated endpoint) in ~/.klaudia/config.toml or ./.klaudia/config.toml; if using apiKeyEnv, export that variable before running")
		}
		p := api.NewOpenAIProvider(cfg.BaseURL, key, cfg.Temperature, extraHeaders)
		p.SetSessionID(sessionID)
		return p, cfg.Model, nil
	default:
		cred, err := api.ResolveCredential()
		if err != nil {
			return nil, "", err
		}
		return api.New(cred, api.ResolveAnthropicBaseURL(cfg.BaseURL)), cfg.Model, nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func buildBrowserOptions(bc config.Browser) browser.Options {
	opts := browser.Options{
		Mode:           browser.ModeLaunch,
		Headless:       true,
		ChromePath:     strings.TrimSpace(os.Getenv("KLAUDIA_CHROME_PATH")),
		UserDataDir:    firstNonEmpty(strings.TrimSpace(os.Getenv("KLAUDIA_CHROME_USER_DATA_DIR")), browser.DefaultUserDataDir()),
		RemoteURL:      strings.TrimSpace(os.Getenv("KLAUDIA_CHROME_REMOTE_URL")),
		HeadedFallback: true,
	}
	if bc.Headless != nil {
		opts.Headless = *bc.Headless
	} else if v := strings.TrimSpace(strings.ToLower(os.Getenv("KLAUDIA_BROWSER_HEADLESS"))); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			opts.Headless = true
		case "0", "false", "no", "off":
			opts.Headless = false
		}
	}
	if bc.ChromePath != "" {
		opts.ChromePath = bc.ChromePath
	}
	if bc.RemoteURL != "" {
		opts.RemoteURL = bc.RemoteURL
	}
	if bc.UserDataDir != "" {
		opts.UserDataDir = bc.UserDataDir
	}
	if bc.HeadedFallback != nil {
		opts.HeadedFallback = *bc.HeadedFallback
	} else if v := strings.TrimSpace(strings.ToLower(os.Getenv("KLAUDIA_BROWSER_HEADED_FALLBACK"))); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			opts.HeadedFallback = true
		case "0", "false", "no", "off":
			opts.HeadedFallback = false
		}
	}
	if bc.SearchEngine != "" {
		opts.SearchEngine = bc.SearchEngine
	}
	if opts.RemoteURL != "" {
		opts.Mode = browser.ModeAttach
	}
	return opts
}

// buildExecutor selects the Bash execution backend from config. Both "os"
// (host confinement via sandbox-exec/bwrap) and "container" modes fall back
// to the local executor when the required tool is absent, cannot run, or the
// config is incomplete (warn explains why) - unless sandbox.failIfUnavailable
// is set, in which case that is an error and Klaudia does not start.
func buildExecutor(sb config.Sandbox, warn func(string)) (sandbox.Executor, error) {
	fallback := func(reason string) (sandbox.Executor, error) {
		if sb.FailIfUnavailable {
			return nil, fmt.Errorf("sandbox mode %q is required (sandbox.failIfUnavailable) but %s", sb.Mode, reason)
		}
		warn(fmt.Sprintf("sandbox mode %q: %s; falling back to local (unconfined) execution", sb.Mode, reason))
		return sandbox.NewLocal(), nil
	}
	switch sb.Mode {
	case config.SandboxOS:
		return buildOSExecutor(sb, fallback)
	case config.SandboxContainer:
		return buildContainerExecutor(sb, fallback)
	default:
		return sandbox.NewLocal(), nil
	}
}

// buildOSExecutor picks the OS-native confinement tool for the current
// platform: sandbox-exec on macOS, bubblewrap on Linux.
func buildOSExecutor(sb config.Sandbox, fallback func(string) (sandbox.Executor, error)) (sandbox.Executor, error) {
	switch goruntime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			return fallback("sandbox-exec not found")
		}
		s := sandbox.NewSeatbelt(sb.WriteRoots, sb.Network)
		s.Hide, s.Keep = hiddenCredentials(sb)
		return s, nil
	case "linux":
		if _, err := exec.LookPath("bwrap"); err != nil {
			return fallback("bwrap (bubblewrap) not found")
		}
		// Installed is not the same as usable. Where unprivileged user
		// namespaces are disabled, bwrap is on PATH and fails every command,
		// so sandbox mode "os" turned each Bash call into an error.
		if out, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "true").CombinedOutput(); err != nil {
			return fallback("bwrap is installed but cannot run here (" + strings.TrimSpace(firstLine(string(out))) + ")")
		}
		b := sandbox.NewBwrap(sb.WriteRoots, sb.Network)
		b.Hide, b.Keep = hiddenCredentials(sb)
		return b, nil
	default:
		return fallback("not supported on " + goruntime.GOOS)
	}
}

// hiddenCredentials is what OS confinement hides from commands: the user's
// credential files and directories, unless sandbox.readCredentials is set.
func hiddenCredentials(sb config.Sandbox) (hide, keep []string) {
	if sb.ReadCredentials {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	return trust.CredentialPaths(home)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func buildContainerExecutor(sb config.Sandbox, fallback func(string) (sandbox.Executor, error)) (sandbox.Executor, error) {
	runtime := sb.Runtime
	if runtime == "" {
		runtime = "docker"
	}
	switch {
	case sb.Image == "":
		return fallback("no image is set")
	case !sandbox.RuntimeAvailable(runtime):
		return fallback(runtime + " is not installed")
	default:
		return sandbox.NewContainer(runtime, sb.Image, sb.MountCWDOr(true), sb.ReadOnly, sb.Network), nil
	}
}

// limitMemory applies sandbox.memoryMax to the Bash executor. A container gets
// --memory; anything else on Linux runs inside a systemd scope with MemoryMax,
// once probe has shown the limit would be enforced. Where it would not be, warn
// says why and commands run without a limit: the setting is protection against
// a runaway command, and refusing to run any command at all is the worse
// failure for it.
func limitMemory(ctx context.Context, e sandbox.Executor, memoryMax, goos string,
	probe func(context.Context, int64) error, warn func(string)) sandbox.Executor {
	if memoryMax == "" {
		return e
	}
	bytes, err := sandbox.ParseMemory(memoryMax)
	if err != nil {
		warn("sandbox.memoryMax ignored: " + err.Error())
		return e
	}
	if c, ok := e.(*sandbox.Container); ok {
		c.Memory = bytes
		return c
	}
	if goos != "linux" {
		warn("sandbox.memoryMax is not supported on " + goos + " outside container mode; commands run without a memory limit")
		return e
	}
	if err := probe(ctx, bytes); err != nil {
		warn("sandbox.memoryMax not applied: " + err.Error() + "; commands run without a memory limit")
		return e
	}
	return sandbox.NewMemoryScope(e, bytes)
}

// mcpController adapts the mcp.Manager to the TUI's MCPController so /mcp can
// inspect and reconnect/disconnect servers without the TUI owning the manager.
type mcpController struct {
	mgr *mcp.Manager
	ctx context.Context
}

func (c mcpController) Servers() []tui.MCPServerInfo {
	// Count live tools per server (mcp__<server>__<tool>).
	counts := map[string]int{}
	for _, t := range c.mgr.Tools(c.ctx) {
		n := t.Name()
		const pfx = "mcp__"
		if !strings.HasPrefix(n, pfx) {
			continue
		}
		if i := strings.Index(n[len(pfx):], "__"); i >= 0 {
			counts[n[len(pfx):len(pfx)+i]]++
		}
	}
	out := make([]tui.MCPServerInfo, 0)
	for _, s := range c.mgr.Servers() {
		info := tui.MCPServerInfo{Name: s.Name, Connected: s.Connected(), Tools: counts[s.Name]}
		if err := s.ListError(); err != nil {
			info.ListErr = err.Error()
		}
		out = append(out, info)
	}
	return out
}

func (c mcpController) Reconnect(name string) error  { return c.mgr.Reconnect(name) }
func (c mcpController) Disconnect(name string) error { return c.mgr.Disconnect(name) }

// turnSettings is the part of a run that one frontend changes between turns and
// another pins for the whole process. The TUI reads Model and ExtraDirs live
// from its Session so /model and /add-dir take effect on the next turn. Every
// non-interactive mode resolves them once at startup.
//
// Permission is here for the frontends with one fixed mode. The two that vary
// it — the TUI and ACP, where the mode is per editor session — supply it on the
// Turn instead, which also makes it live: see agent.Turn.Mode.
//
// It exists so that baseOptions can be shared: these were the only fields the
// per-mode options blocks actually disagreed about, and keeping a whole
// duplicated block per mode in order to vary three values is what let the other
// dozen drift apart unnoticed.
type turnSettings struct {
	Model      anthropic.Model
	Effort     string // the TUI reads /effort per turn; other modes pin it
	Permission permission.Context
	ExtraDirs  []string
}

// mcpReloadNotifier carries reload outcomes from the config watcher to whichever
// frontend is listening.
//
// The two run on different goroutines and the watcher starts first — it is
// wired before the frontend exists — so the listener is registered late and the
// emit side tolerates there being nobody home.
type mcpReloadNotifier struct {
	mu sync.Mutex
	fn func(mcp.ReloadEvent)
}

func (n *mcpReloadNotifier) register(fn func(mcp.ReloadEvent)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fn = fn
}

func (n *mcpReloadNotifier) emit(ev mcp.ReloadEvent) {
	n.mu.Lock()
	fn := n.fn
	n.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// gitBranch returns the current git branch for dir, or "" if not a repo.
func gitBranch(dir string) string {
	out, err := gitprobe.Command(dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitCommit returns the short HEAD commit hash, or "" outside a repo.
func gitCommit(dir string) string {
	out, err := gitprobe.Command(dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const starterConfig = `# Klaudia config
# Global: ~/.klaudia/config.toml ($KLAUDIA_CONFIG_DIR/config.toml when set)
# Local:  ./.klaudia/config.toml (overrides global settings)

# Provider: "anthropic" (the default) or "openai" (any OpenAI-compatible
# endpoint — see the block below). Anthropic reads its key from the
# environment, never from this file:
#   export ANTHROPIC_API_KEY="sk-ant-..."   # or ANTHROPIC_AUTH_TOKEN
# In a local config, delete this line to keep the provider set globally.
provider = "anthropic"

# Default model; --model overrides it per run. Unset uses Klaudia's default.
# model = "sonnet" # haiku | sonnet | opus, or a full model ID

# To use an OpenAI-compatible endpoint instead, replace the provider line
# above with these lines (delete the leading "#" from each), then export the
# variable named in apiKeyEnv. The model here replaces the one above.
#
#   provider = "openai"
#   # The id the endpoint lists (GET <baseURL>/models), sent exactly as
#   # written — no "openai/" prefix unless the endpoint's own ids have one.
#   model = "gpt-5.5"
#   baseURL = "https://api.example.com/v1"
#
#   # apiKeyEnv is the NAME of the environment variable that holds your key:
#   # pick any name, then export a variable of that name, e.g. for this one:
#   #   export MY_API_KEY="sk-..."
#   # Or set apiKey = "sk-..." inline — but the env form keeps secrets out
#   # of files.
#   apiKeyEnv = "MY_API_KEY"

# Temperature (OpenAI-compatible provider only). Omitted by default, so the
# server picks its own; some providers reject the field.
# temperature = 1.0

# Context window in tokens — drives autocompaction so long sessions don't
# overflow the model upstream ("max_tokens must be at least 1, got -N" or
# "context length exceeded"). Defaults to 200000, which fits Claude; for an
# OpenAI-compatible model set the window it actually exposes. Common values:
# 8192, 16384, 32768, 128000.
# contextWindow = 8192

# Model to fall back to when the model is overloaded (retried once) or not
# found (used for the rest of the session). --fallback-model overrides it.
# fallbackModel = "gpt-5.5-mini"

# TUI theme (Markdown + chrome). /theme switches it for a session.
# theme = "nord" # dracula | gruvbox | tokyo-night | nord | catppuccin

# Session retention: old transcripts under ~/.klaudia/sessions are pruned once
# at startup. The active session is never touched. Both caps are independent —
# a session is pruned if it is older than retentionDays OR beyond the newest
# retentionMax. Defaults: 30 days / 100 sessions. Set a value to -1 to disable
# that cap. "klaudia sessions ls" lists them; "klaudia sessions rm <id>" deletes.
# [sessions]
# retentionDays = 30
# retentionMax = 100

# Optional examples:
# [sandbox]
# mode = "local" # local | os | container
#
# [browser]
# searchEngine = "ddg" # ddg | google
# headless = true
#
# dontAsk runs allow-listed tools and denies the rest without prompting —
# the mode for headless and embedded runs.
# [permissions]
# mode = "autonomous" # autonomous | plan | bypassPermissions
#
# [subagents]
# worktree = true # a writing sub-agent gets its own git checkout (default)
#
# [tui]
# notify = "bell" # attention when a turn finishes / a prompt waits: comma list
#                 # of bell | osc9 | osc777, or "all" / "off". Default: bell.
`

func createConfig(scope, cwd string) (string, error) {
	var path string
	switch scope {
	case "global":
		path = config.GlobalPath()
		if path == "" {
			return "", fmt.Errorf("find config directory: no home directory, and KLAUDIA_CONFIG_DIR is not set")
		}
	case "local":
		path = config.ProjectPath(cwd)
	default:
		return "", usageErrorf("--create-config must be global or local, not %q", scope)
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("config already exists: %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(starterConfig), 0o644); err != nil {
		return "", err
	}
	if scope == "local" {
		// The user is writing this file themselves, so the folder is theirs to
		// trust; without this the starter's provider and permission keys would
		// be ignored the first time it is used.
		if _, err := config.TrustProject(cwd); err != nil {
			return "", fmt.Errorf("trust project: %w", err)
		}
	}
	return path, nil
}

// options holds parsed CLI flags, mirroring the JS commander surface
// (08-entry.js setupCommander). Flags the reference has and Klaudia does not
// are simply absent, not accepted and ignored.
type options struct {
	print             bool
	prompt            string
	promptInteractive string // --prompt-interactive: open the TUI and send this as the first message
	model             string
	effort            string // --effort low|medium|high|xhigh|max
	fallbackModel     string
	outputFormat      string
	inputFormat       string
	permissionMode    string
	allowHostChanges  bool
	dangerouslySkip   bool
	verbose           bool
	maxTurns          int
	maxBudgetUSD      float64       // --max-budget-usd: stop the run past this cumulative cost
	backgroundWait    time.Duration // --background-wait: how long -p waits for background children; 0 exits without waiting
	resume            string        // --resume <session-id>
	continueSession   bool          // --continue
	newSession        bool          // --new-session
	forkSession       bool          // --fork-session
	fullResume        bool          // --full (replay entire transcript, not the summary)
	sessionID         string        // --session-id <id>: the id to record under (embedders)

	partialMessages bool   // --include-partial-messages
	createConfig    string // --create-config global|local
	capabilities    bool   // --capabilities: print the embedding capabilities as JSON and exit
	trustProject    bool   // --trust-project: apply this folder's .klaudia/config.toml in full
	// trustedProjectConfig applies ./.klaudia/config.toml in full for this run
	// only: for a launcher that wrote the file itself.
	trustedProjectConfig bool
	safeMode             bool   // --safe-mode: load nothing the project supplies
	loop                 bool   // --loop: autonomous goal-spec iteration
	maxIterations        int    // --max-iterations: outer-loop cap for --loop
	loopDirty            string // --loop-dirty: refuse|commit|allow over pre-existing changes
	loopNoBranch         bool   // --no-branch: run --loop on the current branch
	loopNoCommit         bool   // --no-commit: --loop leaves its work uncommitted

	systemPrompt       string   // --system-prompt: replace the default system prompt
	appendSystemPrompt string   // --append-system-prompt: append to the system prompt
	mcpConfigs         []string // --mcp-config: extra MCP servers (path or inline JSON), repeatable
	addDirs            []string // --add-dir: extra in-scope directories, repeatable

	askTimeout time.Duration // --ask-timeout: bound on a stream-json can_use_tool wait
}

// resolveResumeID selects the prior session to seed from, if any. An explicit
// --resume/--continue is honored in any mode; the implicit "pick up the most
// recent project session" is an interactive convenience only, so headless (-p)
// and embedding (stream-json) runs stay stateless unless asked.
//
// Sessions are keyed by the project root; the launch directory's own session
// dir is read too, so a session recorded from a subdirectory before sessions
// were keyed by the root is still picked up.
func resolveResumeID(root, cwd string, opts options, interactive bool) (string, error) {
	if opts.newSession && (opts.resume != "" || opts.continueSession) {
		return "", usageErrorf("--new-session cannot be combined with --resume or --continue")
	}
	if opts.resume != "" {
		return opts.resume, nil
	}
	if opts.continueSession {
		if id, ok := session.MostRecent(root, cwd); ok {
			return id, nil
		}
		return "", fmt.Errorf("--continue: no previous session found in this project")
	}
	// A pinned --session-id names a new session, so it opts out of the
	// implicit pick-up just as --new-session does.
	if interactive && !opts.newSession && opts.sessionID == "" {
		if id, ok := session.MostRecent(root, cwd); ok {
			return id, nil
		}
	}
	return "", nil
}

// resumeTranscript is the transcript file to read a resumed session from: the
// located copy (which may live under another working directory's project dir),
// else the project-root path, whose read error then names the missing file.
func resumeTranscript(root, resumeID string) string {
	if p, ok := session.Locate(root, resumeID); ok {
		return p
	}
	return session.ExistingPath(root, resumeID)
}

// resumeMessages is the history a resumed session starts with, and whether it
// was seeded from the persisted compaction summary.
//
// Token-saving resume: when a summary exists and full is not set, the history
// is the summary followed by every message recorded after the transcript's
// last compaction boundary — the summary covers what came before it, and
// nothing covers what came after, so dropping those would lose every turn
// since the last compaction. A transcript written before boundaries were
// recorded has no marker to cut at and resumes from the summary alone, as it
// always did. Without a summary, or with full, the whole transcript is
// replayed.
func resumeMessages(root, resumeID string, full bool) ([]anthropic.BetaMessageParam, bool, error) {
	path := resumeTranscript(root, resumeID)
	if summary, ok := session.ReadSummary(root, resumeID); ok && !full {
		msgs := []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
				"Summary of the earlier conversation in this session:\n\n" + summary)),
		}
		tail, marked, err := session.ReadSinceCompaction(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
		if marked {
			rest, err := agent.MessagesFromEntries(tail)
			if err != nil {
				return nil, false, err
			}
			msgs = append(msgs, rest...)
		}
		return msgs, true, nil
	}
	entries, err := session.Read(path)
	if err != nil {
		return nil, false, err
	}
	msgs, err := agent.MessagesFromEntries(entries)
	return msgs, false, err
}

// vetAutoResume decides whether an implicit resume of id goes ahead, and
// returns the id to resume ("" to start fresh) with the one line that says
// what happened. Auto-resume used to be silent unless the resume banner had
// something else to show, so a week-old conversation came back unannounced and
// its whole history was re-sent on the first turn.
//
//   - A session last active more than maxAge ago (0: no cutoff) is left alone:
//     the notice names it and how to resume it.
//   - A session that ended in a model refusal is left alone too: its context
//     keeps tripping the refusal, so every prompt in the resumed session —
//     even an unrelated one — would refuse.
//   - Otherwise it is resumed, and the notice says which session, how long it
//     is and how old, and how to start fresh instead.
func vetAutoResume(root, id string, maxAge time.Duration, now time.Time) (string, string) {
	path := resumeTranscript(root, id)
	var age time.Duration
	if st, err := os.Stat(path); err == nil {
		age = now.Sub(st.ModTime())
	}
	if maxAge > 0 && age > maxAge {
		return "", fmt.Sprintf("Last session %s was active %s, past autoResumeMaxAge (%s) — starting fresh. --continue or -r %s resumes it.",
			id, humanAgo(age), configDuration(maxAge), id)
	}
	if why := autoResumeVeto(root, id); why != "" {
		return "", why
	}
	entries, _ := session.Read(path)
	messages := fmt.Sprintf("%d messages", len(entries))
	if len(entries) == 1 {
		messages = "1 message"
	}
	return id, fmt.Sprintf("Resumed %s · %s · last active %s · --new-session to start fresh",
		id, messages, humanAgo(age))
}

// autoResumeVeto says why auto-resume should start fresh instead of reviving
// session id, or returns "" when it may resume it.
//
//   - It ended in a model refusal: its context keeps tripping the refusal, so
//     every prompt in the resumed session — even an unrelated one — refuses too.
//   - Its last act was /clear, with nothing said after it (so it is still the
//     newest transcript): reviving it would undo the clear.
func autoResumeVeto(root, id string) string {
	path := resumeTranscript(root, id)
	if entries, err := session.Read(path); err == nil && session.LastTurnRefused(entries) {
		return "Last session ended in a model refusal — starting fresh. Its history is left on disk; --resume " + id + " to see it."
	}
	if session.EndsCleared(path) {
		return "Last session was cleared — starting fresh. --resume " + id + " to reopen it."
	}
	return ""
}

// humanAgo renders an age as "just now" or "3d ago".
func humanAgo(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return humanDuration(d) + " ago"
}

// humanDuration renders d in its largest whole unit: 3d, 5h, 12m, 40s.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// configDuration renders d the way [session] autoResumeMaxAge would spell it:
// whole days as "7d", anything else without zero units ("12h", "1h30m").
func configDuration(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	s := d.String() // "12h0m0s", "1h30m0s", "45s"
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// standingGoal returns the file this session keeps its standing goal in, and
// the goal restored from the resumed session (empty when not resuming or none
// was set). A fork (--fork-session, or --resume A --session-id B) inherits the
// goal and records it under its own id straight away, so resuming the fork
// restores it too.
func standingGoal(root, sessionID, transcriptPath, resumeID string) (path, goal string) {
	if transcriptPath == "" {
		transcriptPath = session.Path(root, sessionID)
	}
	path = session.GoalPathFor(transcriptPath)
	if resumeID == "" {
		return path, ""
	}
	from := session.GoalPathFor(resumeTranscript(root, resumeID))
	goal = session.ReadGoal(from)
	if goal != "" && from != path {
		_ = session.WriteGoal(path, goal)
	}
	return path, goal
}

// chooseSessionID returns the id this run records under and, when it continues
// an existing transcript, that transcript's path (empty for a new file at the
// default location).
//
//   - resuming, no fork: the resumed id, appending to the file it was found in
//     (so a session resumed from another cwd stays in one transcript);
//   - --session-id X: X, which must not name an existing session — unless it
//     is the id being resumed, which is the same as a plain resume;
//   - --resume A --session-id B, or --fork-session: a new id (B, or minted)
//     seeded from A, leaving A untouched;
//   - otherwise a freshly minted id.
func chooseSessionID(root string, opts options, resumeID string) (string, string, error) {
	fork := opts.forkSession || (opts.sessionID != "" && opts.sessionID != resumeID)
	if resumeID != "" && !fork {
		p, _ := session.Locate(root, resumeID)
		return resumeID, p, nil
	}
	if opts.sessionID == "" {
		return uuid.NewString(), "", nil
	}
	if p, exists := session.Locate(root, opts.sessionID); exists {
		return "", "", usageErrorf("--session-id %s: a session with this id already exists (%s); use --resume %s to continue it", opts.sessionID, p, opts.sessionID)
	}
	return opts.sessionID, "", nil
}

// NewRootCommand builds the top-level `klaudia` command.
func NewRootCommand() *cobra.Command {
	var opts options

	cmd := &cobra.Command{
		Use:   "klaudia [prompt]",
		Short: "Klaudia — a locally-buildable, extensible agentic coding tool",
		Long: `Klaudia — a locally-buildable, extensible agentic coding tool.

With no prompt it opens the interactive TUI. A positional prompt is shorthand
for -p: it runs headless, prints the result and exits.

Autonomous goal loop: write a spec with /goal in the TUI (or by hand in
.klaudia/GOAL.md), then iterate on it with /goal run [N] in the TUI, or headless:
  klaudia --loop --permission-mode autonomous [--max-iterations N]
         [--loop-dirty allow|commit|refuse]
There is no "goal" subcommand: "klaudia goal run" would be a prompt.

Shell completion: klaudia completion bash|zsh|fish|powershell
(each prints its install steps with --help).`,
		// The first line is the reference-compatible "2.1.66-klaudia (Klaudia)";
		// the second names this build (commit, dirty tree, commit time).
		Version:       version.Line(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A positional prompt is shorthand for -p "<prompt>". Every word
			// is the prompt, so `klaudia explain this code` needs no quotes.
			if opts.promptInteractive != "" && len(args) > 0 {
				return usageErrorf("--prompt-interactive takes the first message itself; the positional %q would also run it headless (-p) and exit. "+
					"Pass the text only to --prompt-interactive", strings.Join(args, " "))
			}
			if opts.prompt == "" && len(args) > 0 {
				if looksLikeGoalSubcommand(args) {
					return usageErrorf("there is no `goal` subcommand — %q would run as a prompt. "+
						"In the TUI use /goal (write the spec) and /goal run [N]; headless, run the loop with "+
						"klaudia --loop --permission-mode autonomous [--max-iterations N] (see klaudia --help)", strings.Join(args, " "))
				}
				opts.prompt = strings.Join(args, " ")
				opts.print = true
			}
			return run(cmd, &opts)
		},
	}

	// Match commander's `--version` output: "<version> (Klaudia)" with no
	// prefix, followed by the build line.
	cmd.SetVersionTemplate("{{.Version}}\n")

	// `klaudia sessions ls|rm` manages the on-disk transcript store headlessly.
	cmd.AddCommand(newSessionsCommand())

	// A malformed command line is a usage error, not a run failure: nothing
	// happened, and the caller should fix the invocation rather than retry it.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageErrorf("%s", err)
	})
	f := cmd.Flags()
	f.BoolVarP(&opts.print, "print", "p", false, "Non-interactive mode: print result to stdout and exit")
	f.StringVar(&opts.promptInteractive, "prompt-interactive", "", "Open the interactive TUI and send this text as the first message, as if typed (slash commands and @file references work); the session stays open. For orchestrators that start a pre-prompted session. A positional prompt, by contrast, runs headless and exits")
	f.StringVar(&opts.model, "model", "", "Model alias (haiku|sonnet|opus) or full model ID")
	f.StringVar(&opts.effort, "effort", "", "Reasoning effort: low|medium|high|xhigh|max (default: config effort, else the model's own default)")
	f.StringVar(&opts.fallbackModel, "fallback-model", "", "Model to use when the model is overloaded (retried once) or not found (for the rest of the session); overrides config fallbackModel")
	f.StringVar(&opts.outputFormat, "output-format", "text", "Output format: text|json|stream-json")
	f.StringVar(&opts.inputFormat, "input-format", "text", "Input format: text|stream-json|acp (stream-json and acp both drive a persistent agent over stdin; acp speaks the Agent Client Protocol for editors such as Zed)")
	f.StringVar(&opts.permissionMode, "permission-mode", "", "Permission mode: autonomous|plan|bypassPermissions (default: config [permissions] mode, else autonomous). The retired default|acceptEdits|dontAsk are accepted as aliases for autonomous")
	f.DurationVar(&opts.askTimeout, "ask-timeout", streamjson.DefaultAskTimeout, "With --input-format stream-json: how long a control_request (can_use_tool, ask_user, exit_plan) waits for the client's control_response before it is denied (0 = wait forever)")
	f.BoolVar(&opts.allowHostChanges, "allow-host-changes", false, "Non-interactive runs: permit changes to this machine (packages, services, /etc, …) without a human to approve them")
	f.BoolVar(&opts.dangerouslySkip, "dangerously-skip-permissions", false, "Skip all permission checks (sets bypassPermissions)")
	f.BoolVar(&opts.verbose, "verbose", false, "Verbose output (required for stream-json)")
	f.IntVar(&opts.maxTurns, "max-turns", 0, "Limit the number of agentic loop turns (0 = unlimited)")
	f.Float64Var(&opts.maxBudgetUSD, "max-budget-usd", 0, "Stop the run once its cumulative cost reaches this many USD, checked at each turn boundary like --max-turns (0 = unlimited; has no effect on models with no known price)")
	f.DurationVar(&opts.backgroundWait, "background-wait", headlessBackgroundWait, "How long a headless run waits for background sub-agents after its turn ends (0 = warn and exit without waiting)")
	f.StringVarP(&opts.resume, "resume", "r", "", "Resume a session by ID")
	f.BoolVarP(&opts.continueSession, "continue", "c", false, "Resume the most recent session in this directory (interactive runs already do this unless --new-session; headless runs only with this flag)")
	f.BoolVar(&opts.newSession, "new-session", false, "Start a fresh session instead of auto-resuming the most recent session in this directory")
	f.BoolVar(&opts.forkSession, "fork-session", false, "When resuming, start a new session ID (preserves the original)")
	f.BoolVar(&opts.fullResume, "full", false, "When resuming, replay the entire transcript instead of the compacted summary")
	f.StringVar(&opts.sessionID, "session-id", "", "Use this id for the session instead of minting one (must not exist yet; with --resume of another id, forks into it)")
	f.BoolVar(&opts.partialMessages, "include-partial-messages", false, "Include partial message chunks as they arrive (only with --print and --output-format=stream-json)")
	f.BoolVar(&opts.trustedProjectConfig, "trusted-project-config", false, "Apply ./.klaudia/config.toml in full for this run without adding the folder to the trust list — for a launcher that wrote that file itself")
	f.BoolVar(&opts.trustProject, "trust-project", false, "Trust the current folder so its .klaudia/config.toml applies in full (permission mode and rules, trust, sandbox, provider endpoint and keys), and exit")
	f.BoolVar(&opts.safeMode, "safe-mode", false, "Start without anything this project supplies: its .klaudia/config.toml, .mcp.json servers, skills, CLAUDE.md, memory and knowledge. For opening an unfamiliar repository or getting past a broken project config")
	f.BoolVar(&opts.capabilities, "capabilities", false, "Print what this binary supports (stream-json protocol version, control requests, result fields, permission modes) as JSON and exit — for drivers that embed Klaudia (docs/embedding.md)")
	f.StringVar(&opts.createConfig, "create-config", "", "Create a starter TOML config and exit: global (~/.klaudia/config.toml, or $KLAUDIA_CONFIG_DIR/config.toml) or local (./.klaudia/config.toml)")
	f.BoolVar(&opts.loop, "loop", false, "Autonomous loop: iterate against the goal spec (PRD.md or .klaudia/GOAL.md) until complete or --max-iterations. Requires --permission-mode autonomous or --dangerously-skip-permissions.")
	f.StringVar(&opts.loopDirty, "loop-dirty", "allow", "What --loop does about uncommitted changes already in the tree: allow (default: run alongside them, leave them uncommitted, never discard or stage them), commit (commit them to the goal branch first, as their own commit), or refuse (do not start)")
	f.BoolVar(&opts.loopNoBranch, "no-branch", false, "--loop: run on the current branch instead of a klaudia/goal-<slug> branch")
	f.BoolVar(&opts.loopNoCommit, "no-commit", false, "--loop: leave each iteration's work uncommitted (for goals whose output is an artifact, not a diff); progress is tracked in the spec. A spec line `mode: artifact` sets this and --no-branch")
	f.IntVar(&opts.maxIterations, "max-iterations", 0, "Max iterations for --loop (0 = default 10, hard cap 50)")
	f.StringVar(&opts.systemPrompt, "system-prompt", "", "Replace the default system prompt entirely with this text")
	f.StringVar(&opts.appendSystemPrompt, "append-system-prompt", "", "Append this text to the system prompt (after --system-prompt when both are given)")
	f.StringArrayVar(&opts.mcpConfigs, "mcp-config", nil, "Load additional MCP servers from a file path or inline JSON (.mcp.json shape), merged over configured servers (repeatable)")
	f.StringArrayVar(&opts.addDirs, "add-dir", nil, "Add an extra directory the agent may operate in, beyond the working directory (repeatable)")

	// Headless inspection subcommands. These run and exit without starting a
	// session, so they are safe in scripts and CI.
	cmd.AddCommand(newDoctorCommand())
	cmd.AddCommand(newConfigCommand())

	cmd.AddCommand(newLoginCommand())

	return cmd
}

// runState is what run needs to know about a run that failed: whether it got
// as far as the agent (after which the frontend reports its own failures), and
// what there is to put in a result line if it did not.
type runState struct {
	start     time.Time
	sessionID string // set once chosen; empty for a failure before that
	started   bool   // the agent loop, TUI or embedding driver has taken over
}

// run dispatches to --create-config, the --loop goal loop, stream-json
// embedding, headless (-p) or the interactive TUI.
//
// In the JSON output modes, a failure before the agent starts — no
// credential, an incomplete provider config, a bad flag combination — is also
// reported as an is_error result line on stdout. A script parsing stdout
// otherwise got nothing at all: the reason went to stderr as plain text. The
// stderr message is kept, for the person at the terminal and for anything
// that already reads it.
func run(cmd *cobra.Command, opts *options) error {
	format, err := ParseOutputFormat(opts.outputFormat)
	if err != nil {
		// No format to report in, so this one stays stderr-only.
		return usageErrorf("%s", err)
	}
	st := &runState{start: time.Now()}
	err = runFormat(cmd, opts, format, st)
	if err != nil && !st.started && reportsPreRunJSON(opts, format) {
		_ = NewRenderer(format, cmd.OutOrStdout()).Result(preRunFailure(st, err))
	}
	return err
}

// reportsPreRunJSON reports whether a failure before the run starts belongs on
// stdout as a result line: a non-interactive run whose output is JSON. The TUI
// owns the terminal, and --loop only renders text.
func reportsPreRunJSON(opts *options, format OutputFormat) bool {
	if format == FormatText || opts.loop {
		return false
	}
	return opts.print || opts.inputFormat == "stream-json"
}

// preRunFailure is the result line for a run that failed before the agent
// started. The subtype is the one a run failure already uses, so a consumer
// switching on the known subtypes needs nothing new; the exit code still tells
// a usage error (2) from any other failure (1). No request was made, so the API
// time and turn count are zero — and nothing was spent, so total_cost_usd's
// zero is true here, unlike on a completed run (#150).
func preRunFailure(st *runState, err error) ResultMessage {
	return ResultMessage{
		Type:       "result",
		Subtype:    "error_during_execution",
		IsError:    true,
		DurationMS: time.Since(st.start).Milliseconds(),
		Result:     "Error: " + strings.TrimSpace(err.Error()),
		SessionID:  st.sessionID,
		UUID:       uuid.NewString(),
	}
}

// runFormat is run once the output format is known. It sets st.sessionID when
// the session is chosen and st.started when a frontend takes over.
func runFormat(cmd *cobra.Command, opts *options, format OutputFormat, st *runState) error {
	cwd, _ := os.Getwd()
	// Project-scoped state — sessions, memory, project skills — is keyed by
	// the project root (the git top-level), so a launch from a subdirectory
	// shares it with a launch from the top. Everything else, tools included,
	// runs in cwd.
	root := projectRoot(cwd)
	if opts.capabilities {
		// Before config and credentials: a driver probes this on a host that
		// may not be configured yet.
		var modes []string
		for _, m := range permission.SelectableModes() {
			modes = append(modes, string(m))
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(streamjson.GetCapabilities(modes, []string{config.ProviderAnthropic, config.ProviderOpenAI}))
	}
	if opts.createConfig != "" {
		path, err := createConfig(opts.createConfig, cwd)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Created %s\n\nEdit it with your provider/baseURL/model/apiKeyEnv, then export the named API key env var and run klaudia.\n", path)
		return nil
	}
	if opts.trustProject {
		added, err := config.TrustProject(cwd)
		if err != nil {
			return err
		}
		if added {
			fmt.Fprintf(cmd.OutOrStdout(), "Trusted %s: its .klaudia/config.toml now applies in full.\n", cwd)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%s is already trusted.\n", cwd)
		}
		return nil
	}

	// Mode: autonomous --loop | an embedding protocol on stdin (stream-json or
	// ACP) | headless -p | interactive TUI. --loop is non-interactive and
	// renders as text.
	embedded := opts.inputFormat == "stream-json" || opts.inputFormat == "acp"
	interactive := !opts.print && !opts.loop && !embedded
	// attended is the other question, and it is not the same one: whether a
	// human is reachable to answer something. An embedding peer has a user
	// sitting in front of it; it just does not have a terminal of ours. Asking
	// "interactive?" when the real question was "is anyone there?" is why MCP
	// elicitation was advertised to the TUI alone.
	attended := interactive || embedded
	if opts.promptInteractive != "" && !interactive {
		return usageErrorf("--prompt-interactive opens the interactive TUI; it cannot be combined with -p, --loop or --input-format stream-json/acp")
	}
	switch opts.inputFormat {
	case "text", "stream-json", "acp":
	default:
		return usageErrorf("--input-format must be text, stream-json or acp")
	}
	if opts.loop {
		if embedded {
			return usageErrorf("--loop cannot be combined with --input-format %s", opts.inputFormat)
		}
		if format != FormatText {
			return usageErrorf("--loop only supports --output-format text")
		}
		if _, err := gitguard.ParsePolicy(opts.loopDirty); err != nil {
			return usageErrorf("--loop-dirty: %v", err)
		}
	} else if opts.loopNoBranch || opts.loopNoCommit {
		return usageErrorf("--no-branch and --no-commit only apply to --loop")
	}
	if opts.inputFormat == "acp" {
		if format != FormatText {
			// ACP owns stdout: every byte on it is a JSON-RPC frame. A renderer
			// writing there too would interleave with the protocol and the
			// editor would see a parse error rather than an explanation.
			return usageErrorf("--input-format acp does not use --output-format; it speaks JSON-RPC on stdout")
		}
		if opts.print {
			return usageErrorf("--input-format acp cannot be combined with --print: ACP is a persistent session, not a single shot")
		}
	}
	if opts.print && format == FormatStreamJSON && !opts.verbose {
		return usageErrorf("--output-format stream-json requires --verbose")
	}
	if opts.maxTurns < 0 {
		return usageErrorf("--max-turns %d: must be 0 (unlimited) or a positive number of turns", opts.maxTurns)
	}
	if opts.partialMessages && (!opts.print || format != FormatStreamJSON) {
		return usageErrorf("--include-partial-messages only works with --print and --output-format=stream-json")
	}
	// Single-shot -p takes its prompt from the command line, piped stdin, or
	// both; --loop reads a goal spec and stream-json input owns stdin.
	if opts.print && !opts.loop && opts.inputFormat != "stream-json" {
		p, err := resolvePrintPrompt(opts.prompt, cmd.InOrStdin(), pipedStdinWait, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		opts.prompt = p
	}

	ctx := cmd.Context()
	r := NewRenderer(format, cmd.OutOrStdout())

	// Resolve the session: explicit resume by id, auto-resume the most recent
	// project session by default, or start new when requested/no prior session.
	// --fork-session writes to a fresh id while preserving the original.
	var initialMessages []anthropic.BetaMessageParam
	if opts.resume != "" && !session.ValidID(opts.resume) {
		return usageErrorf("--resume %q: not a valid session id", opts.resume)
	}
	if opts.sessionID != "" && !session.ValidID(opts.sessionID) {
		return usageErrorf("--session-id %q: use letters, digits, '-' and '_' (at most 128)", opts.sessionID)
	}
	resumeID, err := resolveResumeID(root, cwd, *opts, interactive)
	if err != nil {
		return err
	}
	// Select the model provider (.klaudia/config.toml: anthropic | openai).
	// A project's own config — .klaudia/config.toml and its .mcp.json files —
	// applies in full only in a trusted folder, or when the launcher that
	// wrote it says so.
	projectTrusted := func() bool { return opts.trustedProjectConfig || config.IsTrustedProject(cwd) }
	cfg, err := config.LoadTrusting(cwd, projectTrusted())
	if err != nil {
		return usageErrorf("config: %v", err)
	}
	if opts.safeMode {
		cfg, err = config.LoadHome()
		if err != nil {
			return usageErrorf("config: %v", err)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "safe mode: this project's .klaudia/config.toml, .mcp.json, skills, CLAUDE.md and memory are not loaded")
	}
	// safeMode loads only the home config, so a project cannot supply MCP
	// servers, skills, CLAUDE.md or memory either.
	loadMCP := func() (mcp.Config, []string, error) {
		if opts.safeMode {
			cfg, err := mcp.LoadGlobalConfig()
			return cfg, nil, err
		}
		return mcp.LoadConfigFor(cwd, projectTrusted())
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}
	// The implicit pick-the-most-recent case is vetted and always announced;
	// an explicit --resume/--continue is the user asking for that session by
	// name, so it is honoured as is.
	if resumeID != "" && opts.resume == "" && !opts.continueSession {
		maxAge, aerr := cfg.Session.MaxAge()
		if aerr != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning:", aerr)
		}
		var notice string
		resumeID, notice = vetAutoResume(root, resumeID, maxAge, time.Now())
		fmt.Fprintln(cmd.ErrOrStderr(), notice)
	}

	sessionID, transcriptPath, err := chooseSessionID(root, *opts, resumeID)
	if err != nil {
		return err
	}
	if transcriptPath == "" {
		// A new session is recorded under the project root, not cwd.
		transcriptPath = session.Path(root, sessionID)
	}
	st.sessionID = sessionID
	if resumeID != "" {
		var fromSummary bool
		initialMessages, fromSummary, err = resumeMessages(root, resumeID, opts.fullResume)
		if err != nil {
			return fmt.Errorf("resume %s: %w", resumeID, err)
		}
		if fromSummary {
			fmt.Fprintln(cmd.ErrOrStderr(), "resuming from compacted summary (--full for the entire transcript)")
		}
		if children, cerr := session.ReadChildren(root, resumeID); cerr == nil {
			deliver, orphaned := session.ReconcileChildren(children)
			for _, c := range orphaned {
				where := ""
				if c.Provenance != "" {
					where = " (cut from " + c.Provenance + ")"
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: sub-agent %s (%s) was still running when the session ended%s\n", c.ID, c.Type, where)
			}
			for _, c := range deliver {
				line := c.Result
				if len(line) > 80 {
					line = line[:80]
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "sub-agent %s finished while the session was closed: %s\n", c.ID, line)
			}
		}
	}

	// Lifecycle hooks, if any are configured. Nil when neither config file
	// declares one, which every hook call site treats as "do nothing". A
	// project's hooks run only once the user has approved that exact set
	// (internal/hooks/trust.go).
	hookRunner := hooks.Load(cwd, sessionID)

	// Prune stale sessions once at startup, best effort. The session opened for
	// this run is passed as the active id so retention can never delete it.
	// Silent unless it removed something and --verbose is set: an automatic
	// cleanup should not be chatty, but a surprised user should be able to see it.
	if removed, perr := session.Prune(sessionRetention(cfg.Sessions), sessionID); perr == nil && opts.verbose && len(removed) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "session retention: pruned %d old session(s)\n", len(removed))
	}

	provider, providerModel, err := buildProvider(cfg, sessionID)
	if err != nil {
		return err
	}

	// --model overrides the config/provider default.
	modelStr := opts.model
	if modelStr == "" {
		modelStr = providerModel
	}
	// Claude's aliases and default model are Anthropic's: another provider
	// gets the model string as written, so it has to have one.
	model := api.ResolveModelFor(cfg.Provider, modelStr)
	if model == "" {
		return usageErrorf("provider %q needs a model: set model in .klaudia/config.toml or pass --model", cfg.Provider)
	}
	if w := api.AliasWarning(cfg.Provider, modelStr); w != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}

	effort, thinking, err := resolveReasoning(opts.effort, cfg, func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) })
	if err != nil {
		return err
	}

	// The model enumeration is the backend's own; take it before the
	// fallback wrapper, which does not list models.
	listModels := modelLister(provider)
	// --fallback-model overrides config fallbackModel; neither set leaves the
	// provider unwrapped.
	provider = api.WithFallback(provider, firstNonEmpty(opts.fallbackModel, cfg.FallbackModel))

	// Resolve the permission mode: --permission-mode flag wins, else the config
	// default ([permissions] mode), else autonomous. --dangerously-skip wins
	// over all.
	modeStr := opts.permissionMode
	if modeStr == "" {
		modeStr = cfg.Permissions.Mode
	}
	if modeStr == "" {
		modeStr = string(permission.ModeAutonomous)
	}
	mode, retired := permission.Resolve(permission.Mode(modeStr))
	if retired {
		fmt.Fprintln(cmd.ErrOrStderr(), "note:", permission.DeprecatedNotice(permission.Mode(modeStr)))
	}
	if !mode.Valid() {
		return usageErrorf("invalid permission mode %q (autonomous|plan|bypassPermissions)", modeStr)
	}
	if opts.dangerouslySkip {
		mode = permission.ModeBypassPermissions
	}

	// --add-dir seeds the extra roots in every mode. Interactive sessions can add
	// more at runtime with /add-dir, so extraDirs is read live (see below); in the
	// non-interactive modes the CLI dirs are the whole set.
	cliExtraDirs := append([]string(nil), opts.addDirs...)
	extraDirs := func() []string { return cliExtraDirs }
	hostGate := &agent.HostGate{
		Roots: func() trust.Roots {
			home, _ := os.UserHomeDir()
			return sessionRoots(home, cwd, extraDirs())
		},
		Ledger: trust.NewLedger(trust.NewRoots(func() string { h, _ := os.UserHomeDir(); return h }(), cwd)),
		// Refusals point the model at the tool that gets an operation approved
		// in one go, rather than leaving it to retry the command.
		DeclareTool: "RequestHostChange",
	}
	// Headless path: the mode is fixed for the lifetime of this command,
	// except that a stream-json peer may change it with a set_permission_mode
	// control request. It is read live, not captured, so that change reaches
	// every holder of permCtx — sub-agents included — at their next tool call.
	liveMode := newModeVar(mode)
	permCtx := permission.Context{Mode: liveMode.Get}

	// Refresh MEMORY.md's links to the .klaudia/memory/*.md detail notes before
	// building the prompt, so recall surfaces them (best-effort; idempotent).
	_ = memory.New(filepath.Join(root, ".klaudia")).SyncLinks()
	// Assemble the full system prompt (base instructions + env context +
	// CLAUDE.md) once for this run, then apply the --system-prompt (replace) and
	// --append-system-prompt (append) overrides. The extra-dirs note is layered on
	// per mode below (per turn in the TUI, since /add-dir can change it).
	sysPrompt := prompt.Compose(prompt.SystemIn(cwd, root, string(model)), opts.systemPrompt, opts.appendSystemPrompt)
	if opts.safeMode {
		sysPrompt = prompt.Compose(prompt.SafeSystem(cwd, string(model)), opts.systemPrompt, opts.appendSystemPrompt)
	}
	// Non-interactive modes have a fixed extra-dirs set (--add-dir only), so the
	// note is baked in once. The TUI re-wraps sysPrompt per turn instead, because
	// /add-dir can change the set mid-session.
	headlessSys := withExtraDirs(sysPrompt, cliExtraDirs)

	// Build the tool registry. Sub-agents draw from the base tools (incl. any
	// MCP tools); the top-level registry adds the Agent tool.
	executor, err := buildExecutor(cfg.Sandbox, func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) })
	if err != nil {
		return err
	}
	executor = limitMemory(ctx, executor, cfg.Sandbox.MemoryMax, goruntime.GOOS, sandbox.ProbeMemoryScope,
		func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) })
	// Lazy browser engine for the web tools, tied to the run context and closed
	// at session end so any launched Chrome is reliably terminated (it launches
	// nothing until a web tool actually runs).
	browserEngine := browser.NewEngine(ctx, buildBrowserOptions(cfg.Browser))
	defer browserEngine.Close()
	// Managed jobs (Bash run_in_background) are session-scoped and tied to the
	// run context; KillAll stops them — and now their whole process groups — at
	// session end. The session id names their log directory so /logs can find
	// them and so a resumed session lands on its own logs.
	jobStore := tools.NewJobStore(ctx, sessionID)
	defer jobStore.KillAll()
	// Lazy language-server pool for code-intel tools (Diagnostics/Definition/
	// References/WorkspaceSymbol). Servers are detected on PATH + toolchain dirs, spawned on
	// first use, and shut down at session end. Not downloaded.
	lspPool := lsp.NewPool(ctx, cwd, cfg.LSP.Disabled, nil)
	defer lspPool.Close()
	regOpts := []tools.RegOption{
		tools.WithBrowserEngine(browserEngine),
		tools.WithJobStore(jobStore),
		tools.WithLSP(lspPool),
	}
	if !interactive {
		// Headless / --loop / stream-json embedding: there is nobody to answer a
		// question, so do not offer AskUserQuestion to the model at all.
		regOpts = append(regOpts, tools.WithoutFrontend())
	}
	base, err := tools.DefaultRegistry(executor, regOpts...)
	if err != nil {
		return err
	}

	// Connect configured MCP servers (.mcp.json) and fold in their tools and
	// resource tools. Best effort: a server failure does not abort the run.
	// A .mcp.json that does not parse yields no servers at all. Discarding
	// that error made the session look like one with no MCP configured — the
	// model reports the server is down, and nothing says why.
	mcpCfg, mcpHeld, mcpCfgErr := loadMCP()
	if mcpCfgErr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: mcp config:", mcpCfgErr)
	}
	if len(mcpHeld) > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", mcpHeldMessage(mcpHeld))
	}
	// --mcp-config adds servers from a path or inline JSON, merged over the
	// on-disk config. The merged servers connect through the same mcp.Connect
	// path and their tools carry the same mcp__<server>__<tool> names, so they
	// are gated for trust exactly like project .mcp.json servers (mcpPermission):
	// allowed only under an enforcing trust posture, asked in interactive modes,
	// and refused where there is nobody to ask. A CLI flag buys no extra trust.
	// A bad value is a usage error — nothing has run yet and the user typed it.
	for _, arg := range opts.mcpConfigs {
		extra, perr := mcp.ParseConfigArg(arg)
		if perr != nil {
			return usageErrorf("--mcp-config %v", perr)
		}
		mcpCfg = mcp.Merge(mcpCfg, extra)
	}
	// Servers log to stderr. Headless and embedded runs forward it, prefixed
	// with the server name, to Klaudia's own stderr (which an embedder drains);
	// the TUI owns the terminal, so interactive runs do not.
	// KLAUDIA_MCP_STDERR=<dir> also keeps a per-server log in any mode.
	if !interactive {
		mcp.SetStderr(cmd.ErrOrStderr())
		defer mcp.SetStderr(nil)
	}
	// Only an attended run gets an elicitor. It is what makes Klaudia advertise
	// the elicitation capability, and a server told the user can be asked for an
	// API key will wait for one that is never coming when there is no frontend
	// to ask. Unattended servers see no capability and take their own
	// non-interactive path. Note this is `attended`, not `interactive`: the
	// stream-json peer can put a question to someone.
	var elicitor *mcp.Elicitor
	if attended {
		elicitor = mcp.NewElicitor()
	}
	mcpMgr, mcpErrs := mcp.Connect(ctx, mcpCfg, elicitor)
	defer mcpMgr.Close()
	for _, e := range mcpErrs {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", e)
	}
	// Tell the model too. A server that failed to connect contributes no
	// tools, and without this the model could only conclude the tools never
	// existed - and say so, or improvise - rather than tell the user a
	// configured server is down.
	var downServers []string
	for _, s := range mcpMgr.Servers() {
		if !s.Connected() {
			downServers = append(downServers, s.Name)
		}
	}
	if len(downServers) > 0 {
		sysPrompt += "\n\n# MCP servers unavailable\nThese configured MCP servers failed to connect at startup, so their tools are missing: " +
			strings.Join(downServers, ", ") + ". If the task needs one, tell the user; they can retry it with /mcp."
	}
	// Persistent memory: one store shared by the Memory tool (agent + sub-agents)
	// and the /memory command.
	staticTools := base.All()
	memStore := memory.New(filepath.Join(root, ".klaudia"))
	if memTool, merr := tools.NewMemoryForProject(memStore, root); merr == nil {
		staticTools = append(staticTools, memTool)
	}

	// Skills (bundled, overlaid by the user's skill directories and then the
	// project's) become a single Skill tool the model can invoke; the TUI also
	// dispatches /<skill>. In safe mode the project's skills are left out.
	skillWarn := func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) }
	skills := skill.LoadProject(root, cwd, skillWarn)
	if opts.safeMode {
		skills = skill.LoadUser(skillWarn)
	}
	skillInfos := skillToolInfos(skills)
	if skillTool, serr := tools.NewSkill(skillInfos); serr == nil && skillTool != nil {
		staticTools = append(staticTools, skillTool)
	}

	// buildTools folds the live MCP tools into the fixed local set. It runs at
	// startup and again on every config reload, so the two cannot drift: a tool
	// set assembled one way at boot and another way on reload is how a reloaded
	// session ends up subtly unlike a restarted one.
	//
	// Deferred tool loading: MCP tools can be numerous, so withhold them from
	// the initial request behind a ToolSearch tool that loads them on demand.
	buildTools := func() ([]tools.Tool, map[string]bool) {
		all := append([]tools.Tool(nil), staticTools...)
		all = append(all, mcpMgr.Tools(ctx)...)
		if rts, rerr := mcpMgr.ResourceTools(); rerr == nil && len(mcpMgr.Servers()) > 0 {
			all = append(all, rts...)
		}
		deferred := map[string]bool{}
		present := map[string]bool{}
		for _, t := range all {
			present[t.Name()] = true
			if server, ok := mcpServerOf(t.Name()); ok && !mcpMgr.AlwaysLoad(server) {
				deferred[t.Name()] = true
			}
		}
		// Defer the rarely-used local tools (Browser*, Task*) too, so a session
		// with no MCP servers still trims its standing tool list. Guarded by
		// presence: a tool the registry did not build (e.g. dropped by an
		// option) is not marked deferred.
		for _, name := range tools.DefaultDeferredTools() {
			if present[name] {
				deferred[name] = true
			}
		}
		if len(deferred) > 0 {
			catalog := make([]tools.ToolInfo, 0, len(all))
			for _, t := range all {
				desc, _ := t.Description(ctx)
				catalog = append(catalog, tools.ToolInfo{Name: t.Name(), Description: desc})
			}
			if ts, terr := tools.NewToolSearch(catalog); terr == nil {
				all = append(all, ts)
			}
		}
		return all, deferred
	}

	baseTools, deferredTools := buildTools()
	for _, s := range mcpMgr.Servers() {
		if err := s.ListError(); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: mcp %q connected but its tools could not be read: %v\n", s.Name, err)
		}
	}
	base = tools.NewRegistry(baseTools...)

	// deferredTools is replaced when the MCP config reloads, on the watcher's
	// goroutine, while per-turn closures below read it on the session's. Reads
	// go through currentDeferred; the map is swapped whole, never mutated.
	var deferredMu sync.RWMutex
	currentDeferred := func() map[string]bool {
		deferredMu.RLock()
		defer deferredMu.RUnlock()
		return deferredTools
	}

	// Headless has no one to ask. Ordinary work still runs; host changes are
	// refused with the flag that would permit them, so the output says what to
	// do rather than only what failed.
	approver := agent.HeadlessApprover(opts.allowHostChanges)
	agentTypes := subagent.Load(cwd, func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) })
	wiring, err := withAgentTool(base, provider, opts.maxTurns, deferredTools, cwd, hostGate, agentTypes, cfg.SubagentWorktrees())
	if err != nil {
		return err
	}
	wiring.spawner.WithProviderName(cfg.Provider)
	registry := wiring.registry
	// Sub-agents inherit the hooks for the same reason they inherit the host
	// gate: a child must not be the way around a rule the user set.
	wiring.spawner.WithHooks(hookRunner)

	// rebuildTools reassembles both tool registries from the live MCP state. It
	// is called from two goroutines — the config watcher and the MCP client's
	// notification handler — so a mutex serialises them; a reload racing a
	// server's tools/list_changed must not interleave two Replace calls.
	//
	// Both registries are rebuilt: the main loop dispatches from `registry`,
	// while sub-agents draw from `base`. Updating one and not the other would
	// give a sub-agent a different tool set than its parent.
	var rebuildMu sync.Mutex
	rebuildTools := func() {
		rebuildMu.Lock()
		defer rebuildMu.Unlock()
		next, deferred := buildTools()
		base.Replace(next...)
		registry.Replace(append(append([]tools.Tool(nil), next...), wiring.agentTool)...)
		wiring.spawner.SetDeferred(deferred)
		deferredMu.Lock()
		deferredTools = deferred
		deferredMu.Unlock()
	}

	// tools/list_changed: when a server announces its tool list changed, re-list
	// it and rebuild the registries live, so a server that gains or drops tools
	// mid-session is reflected without a config edit or restart. The Manager
	// installs the notification handler per session, so reconnects stay covered.
	mcpMgr.SetToolsChangedHandler(rebuildTools)

	// MCP config hot reload. An edit to any .mcp.json that applies here takes
	// effect in this session instead of at the next start — installing a server
	// and then having to restart to use it is the whole problem.
	//
	// A config that no longer parses is left alone rather than applied — a
	// half-typed file should not take working servers away mid-session — and
	// that, along with any server that failed to launch, is reported through
	// mcpReloads so a broken edit is not silently indistinguishable from a
	// working one.
	mcpReloads := &mcpReloadNotifier{}
	stopWatch, werr := mcp.Watch(cwd, func() {
		// Trust is re-read, so `klaudia --trust-project` run elsewhere takes
		// effect at the next edit without a restart.
		cfg, held, lerr := loadMCP()
		if lerr != nil {
			mcpReloads.emit(mcp.ReloadEvent{ConfigErr: lerr.Error()})
			return
		}
		errs := mcpMgr.Reload(ctx, cfg)
		if len(held) > 0 {
			errs = append(errs, errors.New(mcpHeldMessage(held)))
		}
		rebuildTools()
		if len(errs) > 0 {
			msgs := make([]string, 0, len(errs))
			for _, e := range errs {
				msgs = append(msgs, e.Error())
			}
			mcpReloads.emit(mcp.ReloadEvent{ServerErrs: msgs})
		}
	})
	if werr == nil {
		defer stopWatch()
	}
	// The TUI registers its own listener once its model exists and prints into
	// the transcript. Every other mode gets stderr, which is where its other
	// diagnostics already go — and is not stdout, so a notice cannot land in the
	// middle of a stream-json line.
	if !interactive {
		mcpReloads.register(mcpReloadWriter(cmd.ErrOrStderr()))
	}

	// Open the transcript for this session (best effort: a transcript failure
	// should not abort the run).
	// The TUI's /clear rotates it to a new session id, so what records or names
	// the session mid-run reads it from here rather than from sessionID.
	sessRec := newSessionRecorder(session.Meta{
		SessionID:      sessionID,
		CWD:            cwd,
		Version:        version.Version,
		Build:          version.Get().Summary(),
		GitBranch:      gitBranch(cwd),
		PermissionMode: string(mode),
		Path:           transcriptPath,
	})
	defer func() { _ = sessRec.Close() }()
	recorder := agent.Recorder(sessRec)

	loop := agent.New(provider, registry)
	// From here each frontend reports its own failures in its own format.
	st.started = true

	// rewindFn is nil when no transcript was opened, which is how
	// tui.Session.Rewind signals "in-memory only". It drops through the
	// recorder so a rewind after /clear acts on the current transcript.
	var rewindFn func(int) error
	if sessRec.HasTranscript() {
		rewindFn = func(dropMessages int) error {
			_, err := sessRec.DropLastMessages(dropMessages)
			return err
		}
	}

	// live tracks the session this run is currently recording to. It starts at
	// the session chosen at launch and is repointed by the interactive /resume
	// picker (below), so compaction summaries follow the resumed session rather
	// than the one Klaudia started in. Read on the agent goroutine, written on
	// the UI goroutine, so it is guarded.
	liveID, livePath := sessionID, transcriptPath
	var liveMu sync.Mutex

	// persistSummary writes a compaction summary for token-saving resume, under
	// whichever session is live (after /resume, the resumed one). Used by
	// /summary edit; onSummary wraps it for autocompact and /compact.
	persistSummary := func(summary string) error {
		liveMu.Lock()
		id, p := liveID, livePath
		liveMu.Unlock()
		if p != "" {
			return session.WriteSummaryAt(session.SummaryPathFor(p), id, summary, gitCommit(cwd))
		}
		return session.WriteSummary(cwd, id, summary, gitCommit(cwd))
	}
	// onSummary is the fire-and-forget persist used by autocompact and after a
	// /compact; the boundary marks where in the transcript this summary was
	// taken, so a resume can seed from it and still replay the messages after it.
	onSummary := func(summary string) {
		_ = sessRec.MarkCompaction()
		_ = persistSummary(summary)
	}

	// The same guard the goal loop uses (#250), for every frontend. A session
	// in autonomous or bypass never prompts before Bash, so nothing else stops
	// `git checkout -- <file>` from discarding a change the user had made before
	// the session started (#257). Captured once, before any turn runs; outside a
	// repository there is nothing to protect and the guard stays nil.
	var sessionGuard func(tool string, input []byte, cwd string) string
	if base, gerr := gitguard.Capture(cwd); gerr == nil {
		sessionGuard = base.Guard()
	}
	// Background children do not run on the parent's context, so the guard
	// has to be handed over explicitly or it holds only for the foreground.
	wiring.spawner.WithCommandGuard(sessionGuard)

	// One options builder for every frontend.
	//
	// There used to be a separate one per mode, each spelling out its own list
	// of ~15 Options fields, and they had drifted: the stream-json copy was
	// missing Asker, Planner and InitialMessages, so questions, plan approval
	// and --resume were all quietly inert over that transport. The per-turn half
	// now comes from agent.Turn.Apply and the per-mode half from turnSettings,
	// which is small enough to read at a glance and lists only what genuinely
	// differs between modes.
	baseOptions := func(turn agent.Turn, s turnSettings) agent.Options {
		opts := agent.Options{
			WorkingDir:    cwd,
			Model:         s.Model,
			Effort:        s.Effort,
			Thinking:      thinking,
			ProviderName:  cfg.Provider,
			System:        withExtraDirs(sysPrompt, s.ExtraDirs),
			ExtraDirs:     s.ExtraDirs,
			MaxTurns:      opts.maxTurns,
			MaxBudgetUSD:  opts.maxBudgetUSD,
			Diagnostics:   lspPool.Diagnostics,
			ContextWindow: cfg.ContextWindow,
			MaxTokens:     int64(cfg.MaxTokens),
			Permission:    s.Permission,
			Host:          hostGate,
			DeferredTools: currentDeferred(),
			Recorder:      recorder,
			WebTools:      true,
			OnSummary:     onSummary,
			Hooks:         hookRunner,
			CommandGuard:  sessionGuard,
		}
		turn.Apply(&opts)
		// Every frontend collects its finished background sub-agents, scoped to
		// the turn's conversation. This used to be set by the TUI alone, so over
		// -p, stream-json and ACP a background Agent launch was never delivered
		// (#276). Set here, a frontend cannot leave it out by accident.
		bg, conversation := wiring.spawner.Background(), opts.Conversation
		var pendingReport string
		var pendingUsage []*tools.ChildUsage
		take := func() {
			pendingReport, pendingUsage = bg.PendingReportFor(conversation)
		}
		opts.CollectBackground = func() string {
			take()
			return pendingReport
		}
		opts.CollectChildUsage = func() []*tools.ChildUsage { return pendingUsage }
		opts.SubagentEvents = func() []agent.Event { return bg.TakeEvents(conversation) }
		return opts
	}

	// Interactive TUI: the default when not headless and not stream-json input.
	// It drives the same loop, prompting the user to resolve permission asks.
	if interactive {
		// Shared settings so slash commands can read/change them between turns.
		ctxLimit, ctxSource := api.ContextWindowFor(cfg.Provider, string(model), cfg.ContextWindow)
		goalPath, restoredGoal := standingGoal(root, sessionID, transcriptPath, resumeID)
		sess := &tui.Session{
			// Effective model (flag or config default), never "" — otherwise a
			// non-Anthropic provider would wrongly resolve to the Anthropic default.
			SessionID:           sessionID,
			Model:               modelStr,
			ResolvedModel:       string(model),
			Effort:              effort,
			Theme:               themeOrWarn(cfg.Theme, func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) }), // user default (~/.klaudia) overlaid by project; /theme overrides per session
			PermissionMode:      string(mode),
			EnterInserts:        tui.EnterInserts(cfg.Input.Enter, func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) }),
			PromptHistory:       session.NewPromptHistory(session.PromptHistoryPath(cwd), tui.MaxInputHistory),
			Notify:              tui.ParseNotify(cfg.TUI.Notify, func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) }),
			NoTagline:           strings.EqualFold(strings.TrimSpace(cfg.Banner.Tagline), "off"),
			Memory:              memStore,
			Goal:                restoredGoal,
			InitialPrompt:       opts.promptInteractive,
			SaveGoal:            func(g string) error { return session.WriteGoal(goalPath, g) },
			MCP:                 mcpController{mgr: mcpMgr, ctx: ctx},
			OnMCPReload:         mcpReloads.register,
			Skills:              append(tuiSkills(skills, func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "warning:", m) }), mcpPromptCommands(ctx, mcpMgr)...),
			Provider:            providerName(cfg),
			SandboxMode:         sandboxMode(cfg.Sandbox),
			CWD:                 cwd,
			GitBranch:           gitBranch(cwd),
			Agents:              tuiAgents(agentTypes),
			ContextWindow:       ctxLimit,
			ContextWindowSource: ctxSource,
			// Nil unless a transcript was opened; /rewind then edits only the
			// in-memory conversation.
			Rewind: rewindFn,

			Doctor: func() string {
				return doctor.Format(doctor.Run(buildDoctorInput(cfg, model, cwd, root, mcpCfg, hookRunner)))
			},
			// Nil unless the provider can enumerate its models; /model falls
			// back to type-the-id when it is.
			ListModels:       listModels,
			Trust:            tui.NewTrustController(hostGate),
			Jobs:             jobStore,
			Executor:         executor,
			Rotate:           sessRec.Rotate,
			BackgroundAgents: wiring.spawner.Background(),
		}
		// Seed from --add-dir so /add-dir extends that set rather than starting
		// empty, and the host gate reads the combined list live each tool call.
		sess.ExtraDirs = append([]string(nil), cliExtraDirs...)
		// Assigned after the literal so the closures can read sess.Model live:
		// /model updates sess.Model, and compaction must summarize with whatever
		// model is selected now, not the one captured when the session was built.
		sess.Compact = func(ctx context.Context, history []anthropic.BetaMessageParam, focus string) ([]anthropic.BetaMessageParam, string, error) {
			return compactAndPersist(ctx, history, focus, func(ctx context.Context, history []anthropic.BetaMessageParam, focus string) ([]anthropic.BetaMessageParam, string, error) {
				return loop.Compact(ctx, history, api.ResolveModelFor(cfg.Provider, sess.Model), focus)
			}, onSummary)
		}
		// /summary reads the persisted summary and, on edit, writes it back.
		sess.ReadSummary = func() (string, bool) { return session.ReadSummary(cwd, sessionID) }
		sess.SaveSummary = persistSummary
		extraDirs = func() []string { return sess.ExtraDirs }
		// A swappable recorder lets the /resume picker repoint the transcript
		// mid-session without rebuilding the loop. The launch transcript's own
		// defer closes it; a resumed one is closed here (and each swap closes the
		// one it replaces).
		rec := newSwapRecorder(recorder)
		defer func() {
			if cur := rec.current(); cur != recorder {
				closeRecorder(cur)
			}
		}()
		sess.Resume = func(id string) ([]anthropic.BetaMessageParam, error) {
			if !session.ValidID(id) {
				return nil, fmt.Errorf("invalid session id %q", id)
			}
			tp := resumeTranscript(cwd, id)
			entries, rerr := session.Read(tp)
			if rerr != nil {
				return nil, fmt.Errorf("read session %s: %w", id, rerr)
			}
			hist, rerr := agent.MessagesFromEntries(entries)
			if rerr != nil {
				return nil, fmt.Errorf("resume %s: %w", id, rerr)
			}
			// Append new turns to the located transcript so the resumed session
			// stays in one file, exactly as the CLI --resume path does.
			ntr, rerr := session.NewTranscript(session.Meta{
				SessionID:      id,
				CWD:            cwd,
				Version:        version.Version,
				GitBranch:      gitBranch(cwd),
				PermissionMode: sess.PermissionMode,
				Path:           tp,
			})
			if rerr != nil {
				return nil, fmt.Errorf("resume %s: %w", id, rerr)
			}
			closeRecorder(rec.swap(ntr))
			liveMu.Lock()
			liveID, livePath = id, ntr.Path()
			liveMu.Unlock()
			return hist, nil
		}

		runFn := func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
			// An MCP server's elicitation has to reach the same prompt the
			// model's own questions use, and the Asker that reaches it is
			// handed over per turn. Point the elicitor at this turn's.
			elicitor.SetAsker(turn.Asker)
			o := baseOptions(turn, turnSettings{
				Model:     api.ResolveModelFor(cfg.Provider, sess.Model), // resolved fresh each turn
				Effort:    sess.Effort,                                   // read per turn, like the model
				ExtraDirs: sess.ExtraDirs,
				// Permission is not set here: the mode comes with the Turn
				// (tui.Model.liveMode), so a /mode bypass or an approved
				// ExitPlanMode takes effect on the very next tool dispatch
				// inside the running turn rather than at the next TUI turn
				// boundary.
			})
			// The swappable recorder, so /resume can repoint the transcript.
			o.Recorder = rec
			return loop.Run(ctx, o, turn.Emit)
		}
		return tui.Run(ctx, tui.RunFunc(runFn), initialMessages, sess)
	}

	// Stream-json input: drive a persistent agent over stdin/stdout (the
	// embedding channel). Each user message is a turn; permission asks,
	// questions and plan approvals are surfaced as control_request and answered
	// by the peer.
	if opts.inputFormat == "stream-json" {
		driver := streamjson.NewDriver(cmd.OutOrStdout())
		driver.AskTimeout = opts.askTimeout
		driver.SessionID = sessionID
		// A resumed session carries its history into the first turn; without
		// this the transcript was appended to but the model saw none of it.
		driver.History = initialMessages
		driver.Init = &streamjson.Init{
			CWD:            cwd,
			Model:          model,
			PermissionMode: string(mode),
			ResumedFrom:    resumeID,
		}
		driver.SetPermissionMode = streamModeSetter(liveMode, mode, cmd.ErrOrStderr())
		liveModel := newModelVar(model)
		driver.SetModel = liveModel.Set
		runFn := func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
			elicitor.SetAsker(turn.Asker)
			o := baseOptions(turn, turnSettings{
				Model:      liveModel.Get(), // set_model applies from the next turn
				Effort:     effort,
				Permission: permCtx,
				ExtraDirs:  cliExtraDirs,
			})
			// The transcript on disk and the peer's envelope stream are fed
			// from the same Record calls, as in the -p path.
			o.Recorder = multiRecorder{recorder, turn.Recorder}
			return loop.Run(ctx, o, turn.Emit)
		}
		// initialMessages, not nil: an explicit --resume/--continue was being
		// resolved and then dropped on this path, so the agent started a fresh
		// conversation while the CLI reported it had resumed one.
		return driver.Run(ctx, cmd.InOrStdin(), runFn)
	}

	// ACP input: serve the Agent Client Protocol over stdio, so an editor
	// drives Klaudia with its own UI. Same loop, same tools; only the wire
	// format and the permission plumbing differ from stream-json.
	if opts.inputFormat == "acp" {
		runFn := func(ctx context.Context, turn agent.Turn) (agent.Result, error) {
			elicitor.SetAsker(turn.Asker)
			return loop.Run(ctx, baseOptions(turn, turnSettings{
				// Permission comes from the Turn: an ACP client keeps a mode
				// per editor session, so one resolved here would be wrong for
				// every session but the first.
				Model: model,
			}), turn.Emit)
		}
		acpCtxLimit, _ := api.ContextWindow(string(model), cfg.ContextWindow)
		srv := acp.New(acp.Options{
			Run:  runFn,
			CWD:  cwd,
			Mode: mode,
			// The editor's first thread *is* the session the CLI resolved, so
			// --resume/--continue reaches ACP: same id, same transcript, same
			// seeded history.
			SessionID: sessionID,
			History:   initialMessages,
			// Per session, not the process-wide recorder baseOptions installs.
			// An editor opens several threads and each is its own conversation;
			// one transcript for all of them interleaves them on disk. Turn.
			// Recorder overrides the process one for every ACP turn, so the
			// outer transcript stays unused — and because a Writer creates its
			// file on the first append, an unused one leaves nothing behind.
			Transcript:    acpTranscript(cwd, mode),
			LoadHistory:   acpLoadHistory(cwd),
			ListSessions:  func() []acp.SessionSummary { return acpSessions(cwd) },
			Commands:      acpCommands(skills),
			ContextWindow: acpCtxLimit,
			Log:           func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "acp:", m) },
		}, cmd.OutOrStdout())
		return srv.Serve(ctx, cmd.InOrStdin())
	}

	// Autonomous goal loop: iterate against the spec until complete or capped.
	if opts.loop {
		return runGoalLoop(ctx, cmd, loopRun{
			loop:         loop,
			cwd:          cwd,
			mode:         mode,
			model:        model,
			provider:     cfg.Provider,
			effort:       effort,
			thinking:     thinking,
			system:       headlessSys,
			maxTurns:     opts.maxTurns,
			maxBudgetUSD: opts.maxBudgetUSD,
			iterations:   opts.maxIterations,
			dirty:        gitguard.Policy(opts.loopDirty),
			runMode:      goal.RunMode{NoBranch: opts.loopNoBranch, NoCommit: opts.loopNoCommit},
			permCtx:      permCtx,
			hostGate:     hostGate,
			approver:     approver,
			deferred:     deferredTools,
			recorder:     recorder,
			onSummary:    onSummary,
			hooks:        hookRunner,
			render:       r,
			diagnostics:  lspPool.Diagnostics,
		})
	}

	// Single-shot headless run. For stream-json output, emit JS-compatible
	// message envelopes (assistant/user) via an envelope recorder alongside the
	// transcript; the simplified delta events are not used in that mode.
	runRecorder := agent.Recorder(recorder)
	emit := func(ev agent.Event) { _ = r.Event(ev); warnOn(cmd.ErrOrStderr(), format, ev) }
	var partial func(anthropic.BetaRawMessageStreamEventUnion)
	if format == FormatStreamJSON {
		// Serialize envelope + partial writes to the same stream.
		var writeMu sync.Mutex
		out := cmd.OutOrStdout()
		runRecorder = multiRecorder{recorder, newEnvelopeRecorder(out, sessionID)}
		// The envelope recorder emits the conversation, so the renderer is not
		// asked to; notices still go to stderr (withNotices, below), where they
		// do not collide with the JSON stream.
		emit = func(agent.Event) {}
		if opts.partialMessages {
			partial = newPartialEmitter(out, sessionID, &writeMu).emit
		}
	}
	emit = withNotices(emit, cmd.ErrOrStderr())
	headlessOpts := baseOptions(agent.Turn{
		Prompt:   opts.prompt,
		History:  initialMessages,
		Emit:     emit,
		Approver: approver,
	}, turnSettings{Model: model, Effort: effort, Permission: permCtx, ExtraDirs: cliExtraDirs})
	// Three fields the shared builder has no business knowing about: a headless
	// run uses the envelope recorder rather than the plain transcript, it is the
	// only mode that streams raw model events, and its deferred set is fixed at
	// startup because there is no turn boundary for a reload to land on.
	headlessOpts.Recorder = runRecorder
	headlessOpts.PartialMessages = partial
	headlessOpts.DeferredTools = deferredTools
	res, err := loop.Run(ctx, headlessOpts, emit)
	// Background sub-agents the turn launched are owed a later turn, and a -p
	// run has no other: wait for them and deliver their results before the
	// result line, within the run's caps.
	res, err = drainBackground(ctx, wiring.spawner.Background(), res, err, drainLimits{
		maxTurns:     opts.maxTurns,
		maxBudgetUSD: opts.maxBudgetUSD,
		wait:         opts.backgroundWait,
		cwd:          cwd,
		sessionID:    sessionID,
	}, func(ctx context.Context, history []anthropic.BetaMessageParam, maxTurns int, maxBudgetUSD float64) (agent.Result, error) {
		o := headlessOpts
		o.Prompt, o.PromptImages, o.InitialMessages = "", nil, history
		o.MaxTurns, o.MaxBudgetUSD = maxTurns, maxBudgetUSD
		return loop.Run(ctx, o, emit)
	}, func(msg string) {
		// Not through emit: over stream-json that is a no-op (the envelope
		// recorder carries the conversation), and this must not be lost too.
		ev := agent.Event{Type: "warning", Content: msg}
		if format == FormatStreamJSON {
			_ = r.Event(ev)
			return
		}
		warnOn(cmd.ErrOrStderr(), format, ev)
	})

	out := ResultMessage{
		Type:          "result",
		Subtype:       "success",
		IsError:       err != nil,
		DurationMS:    time.Since(st.start).Milliseconds(),
		DurationAPIMS: res.APIDuration.Milliseconds(),
		NumTurns:      res.NumTurns,
		Result:        res.Text,
		StopReason:    res.StopReason,
		SessionID:     sessionID,
		TotalCostUSD:  res.CostUSD, // derived from usage + api pricing table; 0 for unpriced models
		Usage: map[string]any{
			"input_tokens":                res.InputTokens,
			"output_tokens":               res.OutputTokens,
			"cache_read_input_tokens":     res.CacheReadInputTokens,
			"cache_creation_input_tokens": res.CacheCreationInputTokens,
		},
		UUID: uuid.NewString(),
	}
	if err != nil {
		out.Subtype = "error_during_execution"
		out.Result = "Error: " + api.FriendlyError(err)
	} else if agent.TurnEndedEmpty(res.Text) {
		// The turn completed but produced no answer — a refusal, or a limit.
		// Reporting "success" with empty output would let a pipeline treat a
		// refusal as a done task. stop_reason already carries the detail; this
		// makes stdout non-empty and the exit code non-zero to match.
		if note := agent.TurnNote(res.StopReason, false); note != "" {
			out.Subtype = res.StopReason
			out.IsError = true
			out.Result = note
		}
	}
	if rerr := r.Result(out); rerr != nil {
		return rerr
	}
	// Exit codes an automation can branch on. The reason is already in the
	// result payload, so nothing more is printed — only the code differs.
	switch {
	case errors.Is(err, context.Canceled):
		return exitError{ExitInterrupted}
	case err != nil:
		return exitError{ExitError}
	case hostChangeWasBlocked(hostGate):
		// The task needed something on this machine and had no way to ask.
		// A caller can turn that into `--allow-host-changes` by itself; making
		// it indistinguishable from a model failure would leave them guessing.
		return exitError{ExitHostChangeBlocked}
	case res.StopReason == "max_turns":
		return exitError{ExitMaxTurns}
	case res.StopReason == "max_budget":
		return exitError{ExitMaxBudget}
	case agent.TurnEndedEmpty(res.Text) && agent.TurnNote(res.StopReason, false) != "":
		// The model refused or hit a limit and returned nothing. No answer came
		// back, so this is a failure a caller should be able to branch on.
		return exitError{ExitError}
	}
	return nil
}

// goalSubcommandWords are what someone reaching for a `klaudia goal …`
// subcommand types after "goal": the TUI's /goal arguments.
// (`klaudia goal --help` never gets here: cobra answers --help with the root
// help, which describes the loop.)
var goalSubcommandWords = map[string]bool{"run": true, "stop": true, "clear": true}

// looksLikeGoalSubcommand reports whether a positional prompt is really an
// attempt at a goal subcommand ("goal", "goal run 5"), which would otherwise
// run as a headless prompt with that text (#248). A real prompt that starts
// with the word goal ("goal: fix the build") has more to say than this.
func looksLikeGoalSubcommand(args []string) bool {
	words := strings.Fields(strings.Join(args, " "))
	if len(words) == 0 || strings.ToLower(words[0]) != "goal" {
		return false
	}
	switch len(words) {
	case 1:
		return true
	case 2:
		return goalSubcommandWords[strings.ToLower(words[1])]
	case 3:
		_, err := strconv.Atoi(words[2])
		return strings.ToLower(words[1]) == "run" && err == nil
	}
	return false
}

// usageErrorf reports an invocation mistake: a bad flag combination, an
// invalid mode, an unparseable rule. Nothing ran, and the caller should fix the
// command line rather than retry it.
func usageErrorf(format string, args ...any) error {
	return fmt.Errorf("%w%s", exitError{ExitUsage}, fmt.Sprintf(format, args...))
}

// errRendered marks a run error that has already been emitted in the result
// output, so Execute exits non-zero without re-printing it.
var errRendered = fmt.Errorf("run failed")

// hostChangeWasBlocked reports whether the guardrail stopped anything this run.
func hostChangeWasBlocked(g *agent.HostGate) bool {
	for _, r := range g.Reports() {
		if r.Enforced {
			return true
		}
	}
	return false
}

// Execute runs the root command, returning the process exit code.
func Execute() int { return ExecuteContext(context.Background()) }

// ExecuteContext runs the root command against ctx. Cancelling ctx (SIGINT /
// SIGTERM from main) unwinds the run, which tears down background jobs,
// browsers, MCP servers and the transcript through the existing defers.
func ExecuteContext(ctx context.Context) int {
	err := NewRootCommand().ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	// A bare exitError carries a code and no text, because the reason is
	// already in the result payload; errRendered likewise. A usage error wraps
	// an exitError *and* a message, and that message has not been seen yet —
	// so the test is whether there is anything to say, not what type it is.
	if msg := strings.TrimSpace(err.Error()); msg != "" && !errors.Is(err, errRendered) {
		fmt.Fprintln(os.Stderr, "Error:", msg)
	}
	return exitCodeFor(err)
}

// mcpHeldMessage says which project MCP servers were not started, and how to
// start them.
func mcpHeldMessage(held []string) string {
	return fmt.Sprintf("this folder is not trusted, so the MCP servers its .mcp.json names were not started: %s. "+
		"Review the file, then run `klaudia --trust-project` here to start them.", strings.Join(held, ", "))
}

// mcpServerOf returns the server an "mcp__<server>__<tool>" name belongs to.
func mcpServerOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", false
	}
	server, _, found := strings.Cut(rest, "__")
	return server, found
}

// resolveReasoning settles the effort and thinking settings for the run.
// --effort wins over the config; a bad --effort is a usage error, while a bad
// value in a config file is warned about and ignored, as a bad theme is, so a
// typo there does not stop Klaudia starting.
func resolveReasoning(flagEffort string, cfg config.Config, warn func(string)) (effort, thinking string, err error) {
	if flagEffort != "" {
		if effort, err = api.ParseEffort(flagEffort); err != nil {
			return "", "", usageErrorf("--effort: %v", err)
		}
	} else if effort, err = api.ParseEffort(cfg.Effort); err != nil {
		warn("config effort: " + err.Error() + "; using the model's default")
		effort = ""
	}
	if thinking, err = api.ParseThinking(cfg.Thinking); err != nil {
		warn("config thinking: " + err.Error() + "; using the model's default")
		thinking = ""
	}
	return effort, thinking, nil
}
