// Package agent implements the core agentic loop: call the model (streaming),
// process the response, dispatch any tool_use blocks to local tools, append the
// results, and repeat until the model stops or max turns is reached.
//
// This mirrors agentLoop in the JS reference (06-app-ui.js). Phase 1 covers the
// single-agent path with local tools; compaction, sub-agents, and server-side
// tools land in later phases.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/compaction"
	"github.com/greenthread-ai/klaudia/internal/hooks"
	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// Emitter receives streaming events for stream-json output. It is called for
// assistant text, tool_use, and tool_result events as they occur. May be nil.
type Emitter func(event Event)

// Event is a streaming event emitted during a run (stream-json mode).
type Event struct {
	Type      string `json:"type"`                  // "assistant" | "tool_use" | "tool_progress" | "tool_result" | "usage" | "compaction" | "warning" | "notice" | "permission_mode" | "subagent_started" | "subagent_finished"
	Text      string `json:"text,omitempty"`        // assistant text
	ToolName  string `json:"tool_name,omitempty"`   // tool_use / tool_result
	ToolUseID string `json:"tool_use_id,omitempty"` // tool_use / tool_result
	Input     any    `json:"input,omitempty"`       // tool_use input
	Content   string `json:"content,omitempty"`     // tool_result content (what the model sees); compaction/notice text
	IsError   bool   `json:"is_error,omitempty"`    // tool_result error flag
	// HostBlocked marks a tool_result that the host gate stopped, as distinct
	// from a tool that failed. The two look identical to the model — both are
	// error results it must respond to — but they are not the same event for the
	// user. A blocked `2>/dev/null` that the model simply routes around is a
	// non-event, and rendering it as a red failure was reported as Klaudia
	// looking broken while it was in fact working correctly.
	HostBlocked bool `json:"host_blocked,omitempty"`
	// FullContent is the untruncated tool output when the tool clamped Content
	// to protect the context window. Local frontends show this; it never goes
	// back to the model. Empty means Content is already complete.
	FullContent string `json:"full_content,omitempty"`
	// Usage deltas for one inner-loop LLM call, emitted after each streamTurn so
	// the TUI/stream-json frontend can update token counters live during long
	// goal iterations rather than waiting for the whole Run to return. The
	// matching Result fields (NumTurns/InputTokens/OutputTokens) stay
	// authoritative — the TUI reconciles against them at doneMsg so dropped
	// usage events still settle correctly.
	InputDelta  int64 `json:"input_delta,omitempty"`
	OutputDelta int64 `json:"output_delta,omitempty"`
	TurnDelta   int   `json:"turn_delta,omitempty"`
}

// Recorder persists conversation messages to a transcript as the loop runs.
// role is "user" or "assistant"; message is the raw Anthropic message JSON.
type Recorder interface {
	Record(role string, message json.RawMessage) error
}

// Options configures a single Run.
type Options struct {
	Prompt    string
	Model     anthropic.Model
	System    string
	MaxTurns  int   // 0 = unlimited
	MaxTokens int64 // 0 = model-aware default via api.MaxOutputTokensFor
	// MaxConcurrent bounds how many Agent launches one turn runs at once. 0 means
	// the default of 3. Other tools keep their own cap.
	MaxConcurrent int
	// MaxBudgetUSD stops the run once cumulative cost reaches this many USD, the
	// same way MaxTurns stops it on turn count. 0 = unlimited. It can only
	// fire for a model with a known price (see api.CostUSD); an unpriced model
	// (e.g. an OpenAI-compatible endpoint) has cost 0 and is never budget-stopped.
	MaxBudgetUSD float64
	Permission   permission.Context
	// Host is the trust gate: it classifies each tool call and refuses changes
	// to this machine that the user has not agreed to. Nil disables the whole
	// feature, which is what every caller that has not been wired up yet gets.
	Host *HostGate
	// WorkingDir is the project root every tool operates relative to — the
	// directory Bash runs in and the default root for Grep/Glob. Empty means
	// the process cwd, which is only correct for callers that already chdir'd.
	WorkingDir string
	// BeforeEdit is called with the paths a mutating tool is about to change,
	// synchronously, immediately before the tool runs. The frontend uses it to
	// checkpoint the current contents for undo.
	//
	// It has to be synchronous and it has to be here rather than in the event
	// stream: a tool_use event reaches a frontend on a channel, so a snapshot
	// taken when the event arrives can race the write it was meant to precede
	// and capture the new contents.
	BeforeEdit func(tool string, paths []string)
	// CommandGuard, when set, is consulted for every tool call before the host
	// gate and the permission rules, in every permission mode including
	// bypassPermissions: a non-empty return refuses the call with that text.
	// The goal loop uses it to keep the model from discarding uncommitted work
	// that predates the run (gitguard). It is inherited by sub-agents run on
	// this Run's context (see WithCommandGuard).
	CommandGuard func(tool string, input []byte, cwd string) string
	// Interject is polled between turns and after each tool batch. It returns
	// anything the user has typed since the last poll, and whether they asked
	// Klaudia to stop once the current step finishes. Nil means nothing can
	// interrupt, which is the right answer for headless.
	Interject func() Interjection
	// Approver resolves permission "ask" decisions. Supplied by the frontend
	// (headless/TUI/editor/SDK). If nil, DenyAll is used.
	Approver Approver
	// CollectBackground, if set, is polled at the same safe injection points as
	// Interject. It returns a report of background sub-agents that have finished
	// since the last poll (empty when none), which the loop appends as a user
	// message so their results reach the model on the next request. This is what
	// delivers a background Agent-tool launch back to the parent. Nil means the
	// caller has no background sub-agents to collect.
	CollectBackground func() string
	// CollectChildUsage, if set, is polled with CollectBackground and returns
	// what the children just delivered spent, so the parent can fold it in.
	CollectChildUsage func() []*tools.ChildUsage
	// Conversation identifies which conversation this run belongs to, for a
	// frontend with more than one; see Turn.Conversation. It is passed to tools
	// (tools.Context.Conversation) so a background sub-agent's result is
	// delivered to the conversation that launched it.
	Conversation string
	// InitialMessages seeds the conversation when resuming a session. The new
	// Prompt (if any) is appended after them.
	InitialMessages []anthropic.BetaMessageParam
	// PromptImages are image attachments for this turn's user message (a TUI
	// "@image.png" reference, base64-encoded). They ride on the same message as
	// Prompt, as image content blocks after the text.
	PromptImages []tools.ResultImage
	// Recorder, if set, receives each user/assistant message for transcript
	// persistence. May be nil.
	Recorder Recorder
	// WebTools enables the server-side web_search and web_fetch tools (executed
	// by the Anthropic API, not locally).
	WebTools bool
	// Asker, if set, lets interactive tools (AskUserQuestion) prompt the user.
	Asker tools.Asker
	// Planner, if set, handles ExitPlanMode approval.
	Planner tools.Planner
	// ReadText, if set, is where the Read tool gets a text file's contents
	// instead of from disk. See Turn.ReadText for why a frontend would want
	// that; nil reads disk.
	ReadText func(ctx context.Context, path string, line, limit int) (string, error)
	// DeferredTools names tools withheld from the initial request (loaded on
	// demand once ToolSearch reveals them). Typically the MCP tools.
	DeferredTools map[string]bool
	// ContextWindow is the model's context size, used for autocompact
	// thresholds. 0 uses the package default.
	ContextWindow int
	// Effort is output_config.effort ("low" … "max"); "" sends none, which
	// leaves the model at its default. Thinking is api.ThinkingAdaptive,
	// api.ThinkingDisabled, or "" to send no thinking parameter. Both are
	// adjusted per model by api.ApplyReasoning.
	Effort   string
	Thinking string
	// ExtraDirs are the session's additional working directories. They ride
	// here so a child the Agent tool launches can be told about them; the
	// parent's own prompt already carries them (cli.withExtraDirs).
	ExtraDirs []string
	// ProviderName is the configured provider ("" = anthropic). The
	// model-aware MaxTokens default comes from Claude's table only on
	// Anthropic; any other provider gets the conservative unknown-model cap.
	ProviderName string
	// PartialMessages, if set, receives raw model stream events during the main
	// answer turn (not compaction summaries). The CLI wires this to a
	// stream_event emitter when --include-partial-messages is set. Nil by
	// default and for the TUI, so its single-reader invariant is untouched.
	PartialMessages func(anthropic.BetaRawMessageStreamEventUnion)
	// OnSummary, if set, is called with each compaction summary the loop
	// produces (autocompact). The CLI persists it alongside the transcript for
	// token-saving resume. May be nil.
	OnSummary func(summary string)
	// Diagnostics, if set, lets Edit/Write fetch language-server diagnostics for
	// a file they just wrote and append any new problems to their result. The
	// CLI wires it to the LSP pool's Diagnostics method; nil (LSP off, or a
	// caller that has not wired it) makes it a no-op. It is a passthrough into
	// tools.Context — the loop does not decide anything with it.
	Diagnostics tools.DiagnosticsFunc
	// Hooks, if set, runs the user's configured shell commands at the four
	// lifecycle points. Nil — the common case — costs nothing: see hooks.go.
	Hooks *hooks.Runner
	// SubAgent marks a child run spawned by the Agent tool.
	//
	// Its only effect is on hooks, and only on UserPromptSubmit: a sub-agent's
	// prompt was written by the model, not the user, so a hook that pastes the
	// current ticket in front of what the user typed has nothing to attach
	// itself to. The tool hooks do fire for a child's calls, because the
	// alternative is a formatter that runs on every edit except the ones a
	// sub-agent made.
	SubAgent bool
}

// Result is the outcome of a Run.
type Result struct {
	Text         string // concatenated final assistant text
	NumTurns     int
	StopReason   string
	InputTokens  int64
	OutputTokens int64
	// CacheReadInputTokens / CacheCreationInputTokens report prompt-cache usage
	// across the run: tokens served from cache (cheap) vs. tokens written to it.
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	// APIDuration is the time spent waiting on the provider across this Run's
	// requests, including autocompact summaries and requests that failed or
	// were retried after a context overflow. Tool execution is not in it, so
	// against wall time it separates "the model was slow" from "the tools
	// were". Sub-agents' requests are theirs, as their token counts are.
	APIDuration time.Duration
	// CostUSD is the cumulative cost of the run in USD, derived from the token
	// totals above and the per-model price table (api.CostUSD). It is 0 for a
	// model with no known price (unknown/OpenAI-compatible), so a 0 here can mean
	// either "free" or "unpriced" — callers that must tell them apart re-query
	// api.CostUSD for the known flag.
	CostUSD float64
	// Children is what the sub-agents this run launched spent. Their cost is
	// already inside CostUSD — a parent under a budget cannot spend past it
	// by delegating — and the breakdown is here so /cost and the stream-json
	// result can show it rather than only the total.
	Children []ChildUsage
	// Messages is the full conversation after the run (initial + this turn's
	// exchanges), so a caller can carry it forward as InitialMessages for the
	// next turn (used by the stream-json embedding frontend).
	Messages []anthropic.BetaMessageParam
}

// ChildUsage is one sub-agent's spend, folded into the parent's totals.
// Model is the model the child ran on, since the parent's price table does
// not price a child that ran on a different one.
type ChildUsage struct {
	Model                    string
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	APIDuration              time.Duration
	CostUSD                  float64
	NumTurns                 int
}

// Loop drives the agentic loop against an API client and a tool registry.
type Loop struct {
	provider api.Provider
	tools    *tools.Registry

	// childUsage holds the spend of sub-agents dispatched this run, keyed by
	// the tool-use id of the Agent call, so the loop can fold it into the
	// parent's totals after dispatch returns its opaque blocks. Guarded by
	// childMu: a grouped dispatch runs calls concurrently.
	childMu    sync.Mutex
	childUsage map[string]*tools.ChildUsage

	// compactFailures counts automatic compactions that failed in a row. After
	// maxCompactFailures the threshold-driven compaction stops trying — each
	// attempt is a model call that fails the same way — until one succeeds; a
	// forced compaction (the request has already overflowed) always tries.
	compactFailures atomic.Int32

	// prefix is the system prompt, tools and betas of the last main request
	// sent, which /compact reuses (see summaryRequest). Guarded by prefixMu.
	prefixMu  sync.Mutex
	prefix    requestPrefix
	hasPrefix bool
}

// requestPrefix is the part of a request that precedes the messages: the
// tool definitions and system prompt, plus the betas sent with them.
type requestPrefix struct {
	system []anthropic.BetaTextBlockParam
	tools  []anthropic.BetaToolUnionParam
	betas  []string
}

func (l *Loop) rememberPrefix(p requestPrefix) {
	l.prefixMu.Lock()
	defer l.prefixMu.Unlock()
	l.prefix, l.hasPrefix = p, true
}

func (l *Loop) lastPrefix() (requestPrefix, bool) {
	l.prefixMu.Lock()
	defer l.prefixMu.Unlock()
	return l.prefix, l.hasPrefix
}

// summaryRequest builds a compaction summary request carrying the same system
// prompt and tools as the conversation's own requests. The prompt cache is
// keyed on the prefix tools → system → messages, so a summary request without
// them missed the cache on the entire history — the largest request a session
// sends, billed in full at the moment it is largest. The tools also define the
// tool_use blocks the history carries.
func summaryRequest(messages []anthropic.BetaMessageParam, model anthropic.Model, maxTokens int64, p requestPrefix, focus string) anthropic.BetaMessageNewParams {
	req := compaction.BuildSummaryRequest(messages, model, maxTokens, focus)
	req.System = p.system
	req.Tools = p.tools
	req.Betas = p.betas
	if len(req.Betas) == 0 {
		req.Betas = api.DefaultBetas
	}
	return req
}

// maxCompactFailures is how many automatic compactions may fail in a row
// before the threshold-driven one stops being attempted.
const maxCompactFailures = 3

// maxSummaryShrinks bounds how many times a summary request that is itself
// too long is made smaller and retried.
const maxSummaryShrinks = 3

// New builds a Loop over a model provider (Anthropic, OpenAI-compatible, …).
func New(provider api.Provider, registry *tools.Registry) *Loop {
	return &Loop{provider: provider, tools: registry}
}

// Run executes the loop until the model stops calling tools or MaxTurns is hit.
func (l *Loop) Run(ctx context.Context, opts Options, emit Emitter) (Result, error) {
	if opts.CommandGuard == nil {
		opts.CommandGuard = CommandGuardFrom(ctx)
	} else {
		ctx = WithCommandGuard(ctx, opts.CommandGuard)
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		// Model-aware default: the flat 8192 was low enough that an ordinary
		// large write hit the output limit mid-tool-call and was dispatched with
		// empty arguments. api.MaxOutputTokens raises it on models that support
		// more, staying a safe under-approximation of each model's real cap.
		maxTokens = int64(api.MaxOutputTokensFor(opts.ProviderName, string(opts.Model)))
	}

	// Deferred tools are withheld from the request until ToolSearch reveals them.
	betas := api.DefaultBetas
	if opts.WebTools {
		betas = append(append([]string{}, betas...), api.WebToolBetas...)
	}
	revealed := newRevealSet()

	// The system prompt is rebuilt per turn because the permission mode can
	// change mid-run — approving a plan flips it — and the model has to be
	// told; see mode.go.
	var modes modeTracker

	// The loop-breaker counters: how many times each IDENTICAL tool call
	// (name+input) has failed within this Run, and how many consecutive
	// failures of the same SHAPE a tool has produced regardless of input.
	// Together they break a model out of retry loops where either the same
	// call or the same kind of mistake keeps recurring. Behind a mutex because
	// a concurrent group's calls reach them together; see failureState.
	fs := newFailureState()
	// Server-tool exchanges already announced as dropped, so an inherited
	// broken transcript is mentioned once rather than every turn.
	reportedDrops := map[string]bool{}

	// rec records a message and says so, once per run, when the transcript
	// cannot be written. The error used to be discarded: a full disk or a
	// removed sessions directory lost the session without a word, and the
	// user found out only when --continue had nothing to resume.
	recordWarned := false
	rec := func(role string, msg any) {
		if err := record(opts.Recorder, role, msg); err != nil && !recordWarned {
			recordWarned = true
			if emit != nil {
				emit(Event{Type: "warning", Content: fmt.Sprintf(
					"the session transcript could not be written (%v) — this conversation will not be resumable until that is fixed", err)})
			}
		}
	}

	var res Result
	messages := append([]anthropic.BetaMessageParam{}, opts.InitialMessages...)

	// The two hooks that run before anything is sent. SessionStart fires once
	// for the process; UserPromptSubmit on every turn that carries a prompt.
	//
	// Their output is prepended to the user message rather than spliced into the
	// system prompt. Injected context is part of the conversation — it is true
	// at the moment it was gathered, and a branch name or ticket summary that
	// silently updated itself in the system prompt would retroactively rewrite
	// what the model was told three turns ago. Prepending also keeps the cached
	// prefix stable.
	var injected []string
	if opts.Hooks != nil {
		if opts.Hooks.ClaimSessionStart() {
			injected = appendHookContext(injected, fireHooks(ctx, opts, emit, hooks.Input{Event: hooks.SessionStart}))
		}
		if opts.Prompt != "" && !opts.SubAgent {
			hr := fireHooks(ctx, opts, emit, hooks.Input{Event: hooks.UserPromptSubmit, Prompt: opts.Prompt})
			if hr.Blocked {
				// The prompt is not sent. This is the one refusal the model is
				// never told about, because there is nothing to tell: no request
				// was made. The user is told, because they are the one whose
				// message was dropped and the hook they wrote is why.
				reason := strings.TrimSpace(hr.Reason)
				if reason == "" {
					reason = "a UserPromptSubmit hook refused it without giving a reason"
				}
				if emit != nil {
					emit(Event{Type: "notice", Content: "prompt not sent — " + reason})
				}
				return Result{StopReason: "blocked_by_hook", Text: reason, Messages: messages}, nil
			}
			injected = appendHookContext(injected, hr)
		}
	}
	if opts.Prompt != "" || len(opts.PromptImages) > 0 || len(injected) > 0 {
		// Hook context first, then the prompt, then any attached images.
		blocks := promptBlocks(opts.Prompt, injected)
		if len(opts.PromptImages) > 0 {
			blocks = append(blocks, userMessageWithImages("", opts.PromptImages).Content...)
		}
		userMsg := anthropic.NewBetaUserMessage(blocks...)
		messages = append(messages, userMsg)
		rec("user", userMsg)
	}

	halted := false
	// calib corrects the local token estimate using the input sizes the API
	// reports; overflowRecovered ensures the compact-and-retry path fires at
	// most once per Run, so a genuinely oversized request still surfaces.
	var calib compaction.Calibration
	overflowRecovered := false
	for {
		res.NumTurns++

		// A correction the user typed mid-turn goes in before the request is
		// built, so the model sees it while deciding what to do next rather
		// than after it has done it. See steer.go.
		if in := pollInterjection(opts); !in.Empty() {
			if msg, ok := steerMessage(in); ok {
				messages = append(messages, msg)
				rec("user", msg)
				if emit != nil {
					emit(Event{Type: "steer", Content: in.Text})
				}
			}
			halted = halted || in.Halt
		}

		// Deliver any background sub-agent that finished since the last turn, at
		// the same safe point a steer lands: as a user message before the request
		// is built, so the model sees the result while deciding what to do next.
		if report := pollBackground(opts); report != "" {
			l.foldDelivered(opts, &res)
			if msg, ok := backgroundMessage(report); ok {
				messages = append(messages, msg)
				rec("user", msg)
				if emit != nil {
					emit(Event{Type: "background", Content: report})
				}
			}
		}

		// Build the tool list for this turn: eager tools plus any deferred tools
		// revealed so far (via ToolSearch). Rebuilt per turn so reveals take
		// effect on the next request.
		toolParams, terr := l.buildToolParams(ctx, opts.DeferredTools, revealed.snapshot())
		if terr != nil {
			res.Messages = messages
			return res, terr
		}
		if opts.WebTools {
			toolParams = append(toolParams, webToolParams()...)
		}
		mode := permission.CurrentMode(opts.Permission)
		announceMode(mode, emit, &modes)
		system := systemFor(opts.System, mode)

		prefix := requestPrefix{system: system, tools: toolParams, betas: betas}

		// Compaction runs at the top of every turn (docs/compaction.md):
		// microcompact first (cheap, local), then autocompact (model-based) if
		// near the context limit. The summary request carries this turn's
		// system prompt and tools so it shares the conversation's cached prefix.
		messages = l.compact(ctx, messages, opts, emit, &calib, prefix, false, &res.APIDuration)

		// Say so when a web search or fetch has gone missing, before the
		// repair quietly papers over it; see servertool.go.
		announceDroppedServerTools(messages, emit, reportedDrops)

		// Repair any message with empty content (e.g. an old refusal recorded with
		// content: null) before sending — the Anthropic API otherwise rejects the
		// whole request with "messages.<i>.content: Field required".
		params := anthropic.BetaMessageNewParams{
			Model:     opts.Model,
			MaxTokens: maxTokens,
			Messages:  sanitizeMessages(messages),
			System:    system,
			Tools:     toolParams,
			Betas:     betas,
		}
		api.ApplyReasoning(&params, opts.Effort, opts.Thinking)
		l.rememberPrefix(prefix)

		estimateAtSend := compaction.EstimateTokens(messages)
		sent := time.Now()
		assistant, finalText, err := l.streamTurn(ctx, params, emit, opts.PartialMessages)
		res.APIDuration += time.Since(sent)
		if err != nil && api.IsContextOverflow(err) && !overflowRecovered {
			// Every resend of an over-limit conversation fails identically, so
			// without this the session is finished. Summarise and re-send once;
			// a second overflow is a real failure and is reported.
			overflowRecovered = true
			if emit != nil {
				emit(Event{Type: "compaction"})
			}
			messages = l.compact(ctx, messages, opts, emit, &calib, prefix, true, &res.APIDuration)
			if emit != nil {
				emit(Event{Type: "compaction", Content: "context overflowed the model's window — summarised and retried"})
			}
			continue
		}
		if err != nil {
			// A stream that failed after text was shown — cut off by a stall or
			// dropped connection, or interrupted by the user — keeps that text,
			// so the history matches what the user read. See interrupted.go.
			if partial, ok := interruptedPartial(assistant, err); ok {
				messages = append(messages, partial)
				record(opts.Recorder, "assistant", partial)
			}
			res.Messages = messages
			return res, err
		}
		// The response reports the request's true input size: that is the only
		// honest number available, so feed it back into the estimate.
		calib.Observe(estimateAtSend, int(assistant.Usage.InputTokens+
			assistant.Usage.CacheReadInputTokens+assistant.Usage.CacheCreationInputTokens))
		res.StopReason = string(assistant.StopReason)
		res.InputTokens += assistant.Usage.InputTokens
		res.OutputTokens += assistant.Usage.OutputTokens
		res.CacheReadInputTokens += assistant.Usage.CacheReadInputTokens
		res.CacheCreationInputTokens += assistant.Usage.CacheCreationInputTokens
		// Recompute cumulative cost from the running totals (not per-delta) so
		// rounding never accumulates. Unknown models return 0, leaving CostUSD 0.
		res.CostUSD, _ = api.CostUSD(string(opts.Model), api.Usage{
			InputTokens:              res.InputTokens,
			OutputTokens:             res.OutputTokens,
			CacheReadInputTokens:     res.CacheReadInputTokens,
			CacheCreationInputTokens: res.CacheCreationInputTokens,
		})
		// The children's cost was priced against their own models and is not
		// in the token totals above, so it has to be added back each time the
		// parent's own cost is recomputed.
		for _, c := range res.Children {
			res.CostUSD += c.CostUSD
		}
		res.Text = finalText
		// Live usage tick: emit per inner LLM call so frontends can update
		// counters during long iterations. TurnDelta=1 mirrors res.NumTurns
		// being incremented at the top of this loop iteration.
		if emit != nil {
			emit(Event{
				Type:        "usage",
				InputDelta:  assistant.Usage.InputTokens,
				OutputDelta: assistant.Usage.OutputTokens,
				TurnDelta:   1,
			})
		}

		// Add the assistant turn to the running conversation. Persisting to the
		// transcript is DEFERRED until we know it's either final (no tools) or
		// has its tool_result paired up — see the record calls below. Persisting
		// here, then dying mid-dispatch (Ctrl+C / SIGTERM / OOM), would leak an
		// orphan tool_use to disk and poison the next resume.
		messages = append(messages, assistant.ToParam())

		// pause_turn: the API paused a long-running server-side tool (e.g. web
		// search). Re-send the accumulated turn to let it continue. The
		// assistant has no local tool_use to pair, so record it now.
		//
		// It may still carry a server_tool_use whose result hasn't arrived — the
		// paused search itself. That is valid to send back mid-flight, but if the
		// turn is then interrupted, the transcript keeps the unpaired block and
		// every later request 400s on it. sanitizeMessages repairs that shape on
		// the way out (servertool.go, orphanServerToolUseIDs).
		if assistant.StopReason == "pause_turn" {
			rec("assistant", assistant)
			continue
		}

		// Collect tool_use blocks from this turn.
		toolUses := toolUseBlocks(assistant)
		if len(toolUses) == 0 {
			// Final (tool-less) answer: structurally fine on its own, record now.
			rec("assistant", assistant)
			res.Messages = messages
			if halted {
				res.StopReason = "user_halt"
			}
			return res, nil
		}
		if halted {
			// The wrap-up request came back wanting to do more work. The user
			// asked it to stop; honour that rather than let it carry on with a
			// polite acknowledgement.
			rec("assistant", assistant)
			res.StopReason = "user_halt"
			res.Messages = messages
			return res, nil
		}

		// Dispatch tools and append their results. reveal lets ToolSearch mark
		// deferred tools active for subsequent turns.
		reveal := revealed.add
		// A tool_use the model never finished emitting because the turn hit the
		// output-token limit arrives here with empty ("{}") arguments — the
		// stream layer patches its truncated, invalid JSON so Accumulate doesn't
		// crash (see api.repairInvalidToolInputs). Dispatching it would run the
		// tool with no arguments (e.g. Write with no file_path/content, which the
		// user saw as a cryptic schema error and a retry loop). Return an
		// actionable result instead so the model shortens or continues.
		truncID, wasTruncated := truncatedToolUseID(assistant)
		preempt := func(tu anthropic.BetaToolUseBlock) (string, bool) {
			if wasTruncated && tu.ID == truncID {
				return truncatedToolNote(maxTokens), true
			}
			return "", false
		}
		resultBlocks := l.dispatchAll(ctx, toolUses, opts, res.CostUSD, emit, reveal, fs, preempt)
		// A sub-agent's spend lands here, before the budget check, so a parent
		// cannot delegate its way past MaxBudgetUSD.
		l.foldChildren(opts, &res)
		// The aggregate cap, after the per-result one. Results that each pass
		// the 30 KB budget still add up, and a turn is not limited to a few
		// calls; see batchcap.go.
		names := make([]string, len(toolUses))
		for i, tu := range toolUses {
			names[i] = tu.Name
		}
		capMessage(resultBlocks, names)
		toolResultMsg := anthropic.NewBetaUserMessage(resultBlocks...)
		messages = append(messages, toolResultMsg)

		// Now persist BOTH back-to-back. The window for process termination
		// between them is microseconds (two Recorder.Record calls), whereas the
		// dispatch above can be seconds-to-minutes — that gap is where orphans
		// used to land in the transcript.
		rec("assistant", assistant)
		rec("user", toolResultMsg)

		// The second poll point. Between the tool results and the next request
		// is the other place a user message can be appended without splitting a
		// tool_use/tool_result pair — and it is the one that matters, because a
		// long turn is mostly tool batches, not turn boundaries.
		if in := pollInterjection(opts); !in.Empty() {
			if msg, ok := steerMessage(in); ok {
				messages = append(messages, msg)
				rec("user", msg)
				if emit != nil {
					emit(Event{Type: "steer", Content: in.Text})
				}
			}
			halted = halted || in.Halt
		}
		// The second delivery point, mirroring the interjection poll above: a
		// background agent that finished while this turn's tools ran reaches the
		// model on the next request rather than a turn later.
		if report := pollBackground(opts); report != "" {
			l.foldDelivered(opts, &res)
			if msg, ok := backgroundMessage(report); ok {
				messages = append(messages, msg)
				record(opts.Recorder, "user", msg)
				if emit != nil {
					emit(Event{Type: "background", Content: report})
				}
			}
		}
		if halted {
			// One more request so the model can report what it finished, then
			// stop. Ending here instead would leave the user to work out what
			// was completed from the tool results.
			res.StopReason = "user_halt"
		}

		if opts.MaxTurns > 0 && res.NumTurns >= opts.MaxTurns {
			res.StopReason = "max_turns"
			res.Messages = messages
			return res, nil
		}

		// Budget stop, checked here alongside max_turns (end of turn, after the
		// tool batch) so the run stops on a clean boundary rather than mid-turn
		// with dangling tool_use. Like max_turns, it can overshoot by at most the
		// last turn's cost. res.CostUSD is 0 for an unpriced model, so this never
		// trips without a known price.
		if opts.MaxBudgetUSD > 0 && res.CostUSD >= opts.MaxBudgetUSD {
			res.StopReason = "max_budget"
			res.Messages = messages
			return res, nil
		}
	}
}

// elisionSpiller gives microcompact somewhere to put the tool results it
// removes, so an elision stays recoverable: the placeholder names the file and
// the model can read it. It uses the same store as the per-result cap, under
// the same 24h prune.
func elisionSpiller(content string) (string, bool) {
	return tools.Spill("elided", content)
}

// compact applies microcompact then (if near the limit) autocompact to the
// message list. Honors DISABLE_COMPACT / DISABLE_MICROCOMPACT /
// DISABLE_AUTO_COMPACT, matching the JS env switches. The autocompact request's
// duration is added to apiTime.
func (l *Loop) compact(ctx context.Context, messages []anthropic.BetaMessageParam, opts Options, emit Emitter, calib *compaction.Calibration, prefix requestPrefix, force bool, apiTime *time.Duration) []anthropic.BetaMessageParam {
	if os.Getenv("DISABLE_COMPACT") != "" {
		return messages
	}

	// Microcompact rewrites tool results the model has already seen. A model
	// with preserved thinking bound every later thinking block to those bytes,
	// so the rewrite fails the check on every following request (a 400 on
	// enforced accounts) — and no append-only form of pruning exists. On those
	// models the conversation runs unpruned until autocompact replaces it
	// with a summary, which the check does accept.
	if os.Getenv("DISABLE_MICROCOMPACT") == "" && !api.PreservesThinking(string(opts.Model)) {
		if out, res := compaction.Microcompact(messages, elisionSpiller); res.Compacted {
			messages = out
			if emit != nil {
				emit(Event{Type: "compaction", Content: fmt.Sprintf("microcompact: elided %d old tool results (~%d tokens saved)", res.ElidedCount, res.TokensSaved)})
			}
		}
	}

	// The estimate is corrected by what the API actually charged for earlier
	// turns; uncorrected it runs low by more than the safety buffer, which is
	// how a session reached "prompt is too long" without ever tripping this.
	// force is set when the provider has already rejected the request for
	// size: the threshold is moot, we are over it by definition.
	if os.Getenv("DISABLE_AUTO_COMPACT") == "" &&
		(force || (l.compactFailures.Load() < maxCompactFailures &&
			compaction.ShouldAutocompact(calib.Scale(compaction.EstimateTokens(messages)), opts.ContextWindow))) {
		// Autocompact is a model call that runs silently (emit=nil), so signal
		// its start with a contentless "compaction" event — the frontend shows
		// "compacting…" for the duration. Microcompact above is instant and
		// skips this: it only drops the done banner below.
		if emit != nil {
			emit(Event{Type: "compaction"})
		}
		sent := time.Now()
		out, err := l.autocompact(ctx, messages, opts, prefix)
		*apiTime += time.Since(sent)
		if err != nil {
			// Say so: a silent failure here is what made a session that had
			// outgrown its window look merely stuck.
			n := l.compactFailures.Add(1)
			if emit != nil {
				msg := fmt.Sprintf("autocompact failed: %v", err)
				if n >= maxCompactFailures {
					msg += fmt.Sprintf(" — %d failures in a row, so automatic compaction is paused until /compact succeeds", n)
				}
				emit(Event{Type: "compaction", Content: msg})
			}
		} else {
			l.compactFailures.Store(0)
			messages = out
			if emit != nil {
				emit(Event{Type: "compaction", Content: "autocompact: summarized prior conversation"})
			}
		}
	}
	return messages
}

// Compact unconditionally summarizes the conversation via the model and returns
// the replacement history plus the summary text. Used by the TUI's /compact
// command (the loop's own autocompact runs automatically near the context
// limit). A non-empty focus is passed through to the summary request so the
// summary emphasizes what the user named; an empty focus is the default prompt.
// Returns an error if the summary call fails or yields no text.
func (l *Loop) Compact(ctx context.Context, messages []anthropic.BetaMessageParam, model anthropic.Model, focus string) ([]anthropic.BetaMessageParam, string, error) {
	prefix, ok := l.lastPrefix()
	if !ok {
		// No turn has been sent yet in this process (a resumed session
		// compacted straight away): there is no cached prefix to share, but
		// the history's tool_use blocks still want their tools defined. If a tool's
		// description fails, the request goes without tools, as it used to.
		ts, _ := l.buildToolParams(ctx, nil, nil)
		prefix = requestPrefix{tools: ts}
	}
	summary, err := l.summarize(ctx, messages, model, 0, prefix, focus)
	if err != nil {
		return nil, "", err
	}
	l.compactFailures.Store(0)
	return compaction.ReplaceWithSummary(summary), summary, nil
}

// autocompact summarizes the conversation via the model and returns the
// replacement history, or the error that stopped it. The summary request
// carries this turn's cached prefix (system prompt + tools) so it shares the
// conversation's prompt cache. Automatic compaction has no user focus.
func (l *Loop) autocompact(ctx context.Context, messages []anthropic.BetaMessageParam, opts Options, prefix requestPrefix) ([]anthropic.BetaMessageParam, error) {
	summary, err := l.summarize(ctx, messages, opts.Model, int(opts.MaxTokens), prefix, "")
	if err != nil {
		return nil, err
	}
	if opts.OnSummary != nil {
		opts.OnSummary(summary)
	}
	return compaction.ReplaceWithSummary(summary), nil
}

// summarize asks the model for a summary of messages. The request is
// sanitized like any other, since the history is sent as it stands. When the
// summary request is itself too long — the case that matters most, since
// compaction is forced by the conversation overflowing — it is made smaller
// (compaction.ShrinkForSummary) and retried, and the summary then says that
// the oldest part of the conversation is not in it.
func (l *Loop) summarize(ctx context.Context, messages []anthropic.BetaMessageParam, model anthropic.Model, maxTokens int, prefix requestPrefix, focus string) (string, error) {
	dropped := 0
	for attempt := 0; ; attempt++ {
		req := summaryRequest(sanitizeMessages(messages), model, summaryMaxTokens(model, maxTokens), prefix, focus)
		assistant, _, err := l.streamTurn(ctx, req, nil, nil)
		if err != nil {
			if !api.IsContextOverflow(err) || attempt == maxSummaryShrinks {
				return "", err
			}
			smaller, n, ok := compaction.ShrinkForSummary(messages)
			if !ok {
				return "", err
			}
			messages, dropped = smaller, dropped+n
			continue
		}
		summary := finalAssistantText(assistant)
		if summary == "" {
			return "", fmt.Errorf("compaction produced no summary")
		}
		if dropped > 0 {
			summary = fmt.Sprintf("(The oldest %d messages were too long to include in this summary and are not reflected in it.)\n\n%s", dropped, summary)
		}
		return summary, nil
	}
}

// summaryMaxTokens is the output budget for a summary: the session's own cap
// or the model's, bounded to [4096, 16384]. A fixed 4096 was thin for a long
// session's summary, which is all that survives it.
func summaryMaxTokens(model anthropic.Model, configured int) int64 {
	n := configured
	if n <= 0 {
		n = api.MaxOutputTokens(string(model))
	}
	return int64(min(max(n, 4096), 16384))
}

// streamTurn issues one streaming request via the provider, emitting
// assistant-text events as deltas arrive, and returns the assembled message.
// rawSink, if non-nil, receives the raw stream events for partial-message
// output; it is nil for compaction summary turns.
func (l *Loop) streamTurn(ctx context.Context, params anthropic.BetaMessageNewParams, emit Emitter, rawSink func(anthropic.BetaRawMessageStreamEventUnion)) (anthropic.BetaMessage, string, error) {
	sink := api.StreamSink{
		OnText: func(delta string) {
			if emit != nil {
				emit(Event{Type: "assistant", Text: delta})
			}
		},
		OnRawEvent: rawSink,
		OnNotice: func(msg string) {
			if emit != nil {
				emit(Event{Type: "notice", Content: msg})
			}
		},
	}
	assistant, err := l.provider.StreamTurn(ctx, params, sink)
	if err != nil {
		return assistant, "", fmt.Errorf("stream: %w", err)
	}
	return assistant, finalAssistantText(assistant), nil
}

// dispatch runs one tool_use: lookup → permission → validate → execute, and
// returns the tool_result block to append to the conversation.
// unknownToolMsg builds the error for an unrecognised tool name. If the name is
// actually a sub-agent type (a common model mistake — calling "Plan" directly),
// it steers the model to the Agent tool instead of a dead-end "no such tool".
func (l *Loop) unknownToolMsg(name string) string {
	if agentTool, ok := l.tools.Lookup("Agent"); ok {
		if lister, ok := agentTool.(interface{ HasType(string) bool }); ok && lister.HasType(name) {
			return fmt.Sprintf("%q is a sub-agent type, not a tool. Launch it with the Agent tool: "+
				"Agent(subagent_type=%q, prompt=…).", name, name)
		}
	}
	// List the real tool names so a model that's improvised a name (e.g. "Find"
	// when it meant Glob/Grep) sees what's actually on offer and can self-correct
	// — without us having to maintain a fuzzy-match table per typo.
	all := l.tools.All()
	names := make([]string, 0, len(all))
	for _, t := range all {
		names = append(names, t.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Sprintf("No such tool available: %s", name)
	}
	return fmt.Sprintf("No such tool available: %s. Available tools: %s.", name, strings.Join(names, ", "))
}

// repeatedFailureMsg is returned when the model retries an identical tool call
// that has already failed repeatedly, instead of running it again. It tells the
// model plainly to stop repeating and what to try instead.
func repeatedFailureMsg(name string, n int) string {
	msg := fmt.Sprintf("You have already tried this exact %s call %d times and it failed the same way each time. "+
		"Stop repeating it — running it again will not change the result. Do something different.", name, n)
	switch name {
	case "Edit", "NotebookEdit":
		msg += " Use Read to get the file's current exact contents, then build old_string by copying the " +
			"text verbatim from what Read returns (including indentation and blank lines), or take a different approach."
	case "Write":
		msg += " Read the file first to see what is actually there before overwriting it."
	}
	return msg
}

// repeatFailureLimit is how many times an identical tool call (same name and
// input) may fail within one Run before dispatch stops re-running it and
// instead steers the model to change tack. Weaker models otherwise retry the
// exact same failing call indefinitely, burning the whole turn budget.
const repeatFailureLimit = 2

// errStreak counts how many consecutive failures of the same SHAPE a tool has
// produced (same exact error message). It's reset whenever the tool succeeds
// or produces a different error. The point: identical-args retry detection
// misses "same mistake, different values" — like Read called repeatedly with
// `line_start`/`line_end` (wrong field names) but new line numbers each time.
//
// `firstInput`/`varied` track whether the model actually changed its inputs
// across the failing calls. If it did, and the tool actually RAN, the failure
// is almost certainly environmental (shell wedged, network down, disk full) —
// the model is varying its guesses in good faith and still hitting the same
// wall. `preExec` is the veto on that inference: a call rejected before the
// tool ran cannot have been defeated by the environment. shortCircuit uses the
// two flags to choose between the directive messages.
type errStreak struct {
	sig        string // last error message text
	count      int    // consecutive occurrences of sig
	firstInput string // raw JSON of the input on the first failure in this streak
	varied     bool   // true once a same-sig failure arrived with a different input
	preExec    bool   // the failures never reached the tool: bad name, bad input, denied
}

// failureKind says where a failure came from, which is what decides the
// conclusion the loop-breakers are allowed to draw from a run of them.
type failureKind int

const (
	// failureExec: the tool ran and failed. A wedged environment is a candidate.
	failureExec failureKind = iota
	// failurePreExec: refused before the tool ran — unrecognised name, input
	// that doesn't validate, permission denied. Nothing about the environment
	// is implicated, so these must never produce the "shell wedged" directive.
	failurePreExec
	// failureHostGate: refused by the host gate. A decision about one call,
	// not a malfunction; kept out of the shape streak entirely.
	failureHostGate
)

// bumpErrStreak records a new failure for tool. If the message matches the
// prior signature, the count grows and the input-varied flag tracks whether
// the model is changing inputs across calls. Otherwise the streak starts over.
func bumpErrStreak(streaks map[string]errStreak, tool, msg, input string, preExec bool) {
	prev := streaks[tool]
	if prev.sig == msg {
		prev.count++
		if input != prev.firstInput {
			prev.varied = true
		}
		prev.preExec = prev.preExec || preExec
	} else {
		prev = errStreak{sig: msg, count: 1, firstInput: input, preExec: preExec}
	}
	streaks[tool] = prev
}

// repeatedShapeFailureMsg is the directive returned when loop-breaker B fires
// AND the model kept submitting the same input — classic "stop guessing" case.
// Quoting the recurring error forces the model to confront the real problem
// rather than retry with cosmetic variations.
func repeatedShapeFailureMsg(tool string, n int, sig string) string {
	return fmt.Sprintf(
		"%s has failed with the SAME error %d times in a row. Stop calling it until you've understood the error below — guessing different values for the same wrong call won't help.\n\n--- recurring error ---\n%s",
		tool, n, sig,
	)
}

// envFailureMsg is the directive returned when loop-breaker B fires, the model
// HAD varied its inputs across the failing calls, and the tool actually ran
// each time. The error shape is stable while the inputs aren't — the env, not
// the call, is broken. Telling the model to "stop guessing" in this case is
// actively misleading (it WAS trying different things) and pushes it into
// useless workaround loops. Suggest concrete recovery moves instead.
//
// This must not fire for failures that never reached the tool. A rejected input
// is stable across varied inputs for the obvious reason, and answering it with
// "the shell may be wedged, try KillShell" sends the model to reset a substrate
// that was never involved instead of reading the error.
func envFailureMsg(tool string, n int, sig string) string {
	return fmt.Sprintf(
		"%s has failed %d times in a row with the same error shape across DIFFERENT inputs. This looks like an environment issue (shell wedged, leaked background process, network unreachable, filesystem broken) rather than a tool-input problem — varying the inputs more won't help. Options: (a) reset the relevant state (e.g. KillShell for a stuck Bash; restart a stuck server); (b) try a different tool that doesn't depend on the broken substrate; (c) ask the user. Stop retrying %s with new inputs.\n\n--- recurring error ---\n%s",
		tool, n, tool, sig,
	)
}

// editedPaths returns the files a mutating tool is about to change, or nil if
// it is not one. Kept next to dispatch because the list of mutating tools is a
// property of the loop, not of any one tool.
func editedPaths(name string, raw []byte) []string {
	switch name {
	case "Write", "Edit", "NotebookEdit":
	default:
		return nil
	}
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	for _, p := range []string{in.FilePath, in.NotebookPath} {
		if p != "" {
			return []string{p}
		}
	}
	return nil
}

// shortCircuit returns a tool_result without touching the failure counters
// (the loop-breaker is refusing the call, not registering another attempt).
// Used by loop-breaker B; A reuses errResult since it bumps state intentionally.
// truncatedToolUseID reports the id of a tool_use the model never finished
// emitting because the turn hit the output-token limit. max_tokens truncates
// only the final content block, so a trailing tool_use whose arguments never
// closed is the casualty; the stream layer patches its invalid JSON to "{}"
// (api.repairInvalidToolInputs) to keep Accumulate from crashing, which is the
// empty input keyed on here. Only the final block qualifies, so a complete
// tool_use earlier in the same turn still dispatches normally.
func truncatedToolUseID(m anthropic.BetaMessage) (string, bool) {
	// Both stop reasons cut generation off mid-block: max_tokens is the output
	// cap, model_context_window_exceeded is the context limit (Claude 4.5+
	// accepts input+max_tokens>context and stops this way rather than 400ing).
	if len(m.Content) == 0 {
		return "", false
	}
	switch m.StopReason {
	case "max_tokens", "model_context_window_exceeded":
	default:
		return "", false
	}
	last := m.Content[len(m.Content)-1]
	if last.Type != "tool_use" || !isEmptyToolInput(last.Input) {
		return "", false
	}
	return last.ID, true
}

func isEmptyToolInput(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "{}", "null":
		return true
	}
	return false
}

func truncatedToolNote(maxTokens int64) string {
	return fmt.Sprintf("This tool call was cut off before its arguments were complete: "+
		"the turn reached the output-token limit (max_tokens=%d) mid-call, so it did NOT "+
		"run — nothing was written or changed. Produce less in one turn: write a large file "+
		"in smaller pieces (create it, then append/edit the rest), or shorten the content, "+
		"then continue.", maxTokens)
}

// noteChild records one sub-agent's spend against the tool call that
// launched it. An Agent call yields one result, so the first wins.
func (l *Loop) noteChild(toolUseID string, u *tools.ChildUsage) {
	l.childMu.Lock()
	defer l.childMu.Unlock()
	if l.childUsage == nil {
		l.childUsage = map[string]*tools.ChildUsage{}
	}
	if _, ok := l.childUsage[toolUseID]; !ok {
		l.childUsage[toolUseID] = u
	}
}

// takeChildren returns and forgets the sub-agent spend recorded since the
// last call, in no particular order.
func (l *Loop) takeChildren() []*tools.ChildUsage {
	l.childMu.Lock()
	defer l.childMu.Unlock()
	if len(l.childUsage) == 0 {
		return nil
	}
	out := make([]*tools.ChildUsage, 0, len(l.childUsage))
	for _, u := range l.childUsage {
		out = append(out, u)
	}
	l.childUsage = nil
	return out
}

// foldChildren adds the sub-agents launched by the dispatch just finished
// into the parent's result and recomputes cost, so a budget check made
// after this sees the children's spend.
//
// The children's tokens stay out of the parent's token totals: those are
// priced against the parent's model, and a child may have run on another.
// Their cost, already priced against their own model, is added on top.
func (l *Loop) foldChildren(opts Options, res *Result) {
	children := l.takeChildren()
	if len(children) == 0 {
		return
	}
	for _, u := range children {
		res.Children = append(res.Children, ChildUsage{
			Model:                    u.Model,
			InputTokens:              u.InputTokens,
			OutputTokens:             u.OutputTokens,
			CacheReadInputTokens:     u.CacheReadInputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
			APIDuration:              u.APIDuration,
			CostUSD:                  u.CostUSD,
			NumTurns:                 u.NumTurns,
		})
	}
	own, _ := api.CostUSD(string(opts.Model), api.Usage{
		InputTokens:              res.InputTokens,
		OutputTokens:             res.OutputTokens,
		CacheReadInputTokens:     res.CacheReadInputTokens,
		CacheCreationInputTokens: res.CacheCreationInputTokens,
	})
	for _, c := range res.Children {
		own += c.CostUSD
	}
	res.CostUSD = own
}

// Nil means the turn has no budget, so the child is not bounded by one either.
func budgetLeft(opts Options, spentUSD float64) *float64 {
	if opts.MaxBudgetUSD <= 0 {
		return nil
	}
	left := opts.MaxBudgetUSD - spentUSD
	if left < 0 {
		left = 0
	}
	return &left
}

func shortCircuit(emit Emitter, tu anthropic.BetaToolUseBlock, msg string) anthropic.BetaContentBlockParamUnion {
	if emit != nil {
		emit(Event{Type: "tool_result", ToolName: tu.Name, ToolUseID: tu.ID, Content: msg, IsError: true})
	}
	return anthropic.NewBetaToolResultBlock(tu.ID, msg, true)
}

func (l *Loop) dispatch(ctx context.Context, tu anthropic.BetaToolUseBlock, opts Options, spentUSD float64, emit Emitter, reveal func(...string), fs *failureState) anthropic.BetaContentBlockParamUnion {
	raw, _ := json.Marshal(tu.Input)
	if emit != nil {
		emit(Event{Type: "tool_use", ToolName: tu.Name, ToolUseID: tu.ID, Input: tu.Input})
	}

	key := tu.Name + "\x00" + string(raw)
	rawStr := string(raw)
	errResultKind := func(msg string, kind failureKind) anthropic.BetaContentBlockParamUnion {
		fs.fail(key)
		// A refusal by the host gate is a decision, not a malfunction. Its text
		// is identical whatever the command was, so feeding it to the same-shape
		// streak makes two unrelated refused commands look like proof the
		// environment is wedged, and loop-breaker B then latches the tool for
		// the rest of the Run — a latch nothing can clear, because the only
		// reset is a successful execution and B is what prevents one. Count the
		// exact call (breaker A still stops a literal retry) but leave the shape
		// streak out of it.
		if kind != failureHostGate {
			fs.bumpStreak(tu.Name, msg, rawStr, kind == failurePreExec)
		}
		if emit != nil {
			emit(Event{
				Type: "tool_result", ToolName: tu.Name, ToolUseID: tu.ID,
				Content: msg, IsError: true, HostBlocked: kind == failureHostGate,
			})
		}
		return anthropic.NewBetaToolResultBlock(tu.ID, msg, true)
	}
	// errResult: the tool ran and failed.
	errResult := func(msg string) anthropic.BetaContentBlockParamUnion {
		return errResultKind(msg, failureExec)
	}
	// errBadCall: the call was refused before the tool ran, so no conclusion
	// about the environment may be drawn from a run of these.
	errBadCall := func(msg string) anthropic.BetaContentBlockParamUnion {
		return errResultKind(msg, failurePreExec)
	}

	// Loop-breaker A: this exact call has already failed repeatedly. Don't run
	// it again — it would fail identically. Refuse with shortCircuit for the
	// same reason B does: we are declining to run the call, not observing it
	// fail, and recording our own refusal as a failure would let A feed B until
	// B latches the tool outright.
	if n := fs.count(key); n >= repeatFailureLimit {
		return shortCircuit(emit, tu, repeatedFailureMsg(tu.Name, n))
	}
	// Loop-breaker B: the same tool has produced the same KIND of error for
	// `repeatFailureLimit` consecutive calls. Two cases — identical inputs (the
	// model is in a guessing loop) or varied inputs (the environment is wedged
	// and the same error shape comes back regardless). Different directives.
	if streak := fs.streak(tu.Name); streak.count >= repeatFailureLimit {
		// Use shortCircuit() so we don't keep growing the streak on each fired
		// short-circuit (the model isn't actually trying — we're refusing).
		msg := repeatedShapeFailureMsg(tu.Name, streak.count, streak.sig)
		if streak.varied && !streak.preExec {
			msg = envFailureMsg(tu.Name, streak.count, streak.sig)
		}
		// Clear the streak as the directive goes out, because the only other
		// reset is a successful execution and this branch is what prevents one.
		// Left in place it is not a circuit breaker but a latch: a real session
		// reached a state where `true`, `pwd` and `echo hello` were all refused
		// in 0ms for the rest of the run, long after whatever broke had passed.
		// Firing once per streak keeps the anti-loop property — a model that
		// keeps failing gets the directive again after another
		// repeatFailureLimit failures — without making the tool unusable.
		fs.clearStreak(tu.Name)
		return shortCircuit(emit, tu, msg)
	}

	tool, ok := l.tools.Lookup(tu.Name)
	if !ok {
		return errBadCall(l.unknownToolMsg(tu.Name))
	}

	// Validate the arguments FIRST — before the host gate and the approval
	// step. An obviously-malformed call (bad or missing arguments) cannot run
	// whatever the gate or the user decides, so rejecting it here spares the
	// host gate the work and, more importantly, spares the user an approval
	// prompt for a call that was never viable.
	if err := tool.ValidateInput(raw); err != nil {
		msg := fmt.Sprintf("Input validation error: %v", err)
		// Tell the model what the tool actually accepts. Smaller models hallucinate
		// param names ("line_start" instead of "offset") and keep retrying with the
		// same wrong shape — listing the real fields lets them self-correct.
		if fields := schemaFieldList(tool.InputSchema()); fields != "" {
			msg += fmt.Sprintf(" — %s accepts: %s.", tu.Name, fields)
		}
		return errBadCall(msg)
	}

	// The command guard is a data-safety check, not a permission: no mode
	// waives it, bypassPermissions included. The goal loop sets it so the model
	// cannot discard (or sweep into its commits) uncommitted work that predates
	// the run (gitguard, #250). Classified like a host refusal — a decision, not
	// a malfunction — so repeated refusals do not latch the tool.
	if opts.CommandGuard != nil {
		if msg := opts.CommandGuard(tu.Name, raw, opts.WorkingDir); msg != "" {
			return errResultKind(msg, failureHostGate)
		}
	}

	// The host gate runs BEFORE the permission check, not after. The check is
	// per-tool and mode-driven, and in autonomous — the default — it allows
	// everything, so a gate running second would never see the calls that most
	// need it. See hostgate.go.
	//
	// bypassPermissions skips it, because that mode's entire contract is "no
	// checks" and honouring half of it would be worse than honouring none.
	if permission.CurrentMode(opts.Permission) != permission.ModeBypassPermissions {
		hd := opts.Host.Check(tu.Name, raw, opts.WorkingDir)
		switch {
		case hd.Refuse != "":
			return errResultKind(hd.Refuse, failureHostGate)
		case len(hd.Ask) > 0:
			if !l.approveHostChange(ctx, tu, raw, opts, hd) {
				return errResultKind(hostDeclinedMsg(hd), failureHostGate)
			}
			// Approval is new information about the tool's prospects. Anything
			// it was carrying from earlier refusals — this call's own count and
			// the tool's shape streak — is now stale, and leaving it in place
			// is how "permission granted" still leaves the tool unusable.
			fs.clear(key, tu.Name)
		}
	}

	req := tool.PermissionRequest(raw)
	decision := permission.Check(opts.Permission, tool, req)
	switch decision.Behavior {
	case permission.Deny:
		msg := decision.Message
		if msg == "" {
			msg = fmt.Sprintf("Permission denied for tool %s", tu.Name)
		}
		return errBadCall(msg)
	case permission.Ask:
		// Delegate the decision to the frontend's Approver.
		approver := opts.Approver
		if approver == nil {
			approver = DenyAll
		}
		ad := approver.Approve(ctx, ApprovalRequest{
			ToolName:   tu.Name,
			ToolUseID:  tu.ID,
			Input:      raw,
			Specifier:  req.Specifier,
			Suggestion: decision.Message,
		})
		if ad.Behavior != permission.Allow {
			msg := ad.Message
			if msg == "" {
				msg = fmt.Sprintf("Permission denied for tool %s", tu.Name)
			}
			return errBadCall(msg)
		}
	}

	if opts.BeforeEdit != nil {
		if paths := editedPaths(tu.Name, raw); len(paths) > 0 {
			opts.BeforeEdit(tu.Name, paths)
		}
	}

	// PreToolUse. Last, after the gate, the permission check and validation, so
	// a hook is only ever asked about a call Klaudia was otherwise going to
	// make — it can narrow, never widen. See hooks.go.
	//
	// A refusal is classified as a host-gate failure for the same reason the
	// gate's own is: the text is identical every time the hook fires, so feeding
	// it to the same-shape streak would make two unrelated blocked calls look
	// like a wedged environment and latch the tool.
	if hookEnabled(opts, hooks.PreToolUse) {
		hr := fireHooks(ctx, opts, emit, hooks.Input{
			Event:     hooks.PreToolUse,
			ToolName:  tu.Name,
			ToolInput: toolInputFor(raw),
		})
		if hr.Blocked {
			return errResultKind(hookBlockedMsg(tu.Name, hr.Reason), failureHostGate)
		}
	}

	// Progress is only wired when someone is listening. A long-running tool (the
	// Agent tool, which runs a whole child loop) reports through this so the
	// frontend can show movement instead of an unexplained pause.
	var progress func(string)
	if emit != nil {
		progress = func(line string) {
			emit(Event{
				Type:      "tool_progress",
				ToolName:  tu.Name,
				ToolUseID: tu.ID,
				Text:      line,
			})
		}
	}
	results, err := tool.Execute(ctx, tools.Context{
		WorkingDir:   opts.WorkingDir,
		Ask:          opts.Asker,
		Plan:         opts.Planner,
		Reveal:       reveal,
		HostChange:   hostChangeFor(opts),
		Progress:     progress,
		ReadText:     opts.ReadText,
		Diagnostics:  opts.Diagnostics,
		Conversation: opts.Conversation,
		Approver:     opts.Approver,
		Mode:         opts.Permission.Mode,
		Model:        string(opts.Model),
		Effort:       opts.Effort,
		Thinking:     opts.Thinking,
		BeforeEdit:   opts.BeforeEdit,
		ExtraDirs:    opts.ExtraDirs,
		Budget:       budgetLeft(opts, spentUSD),
	}, raw)
	if err != nil {
		return errResult(fmt.Sprintf("Tool execution error: %v", err))
	}

	// Collapse the result text, and collect any image blocks (vision). `full`
	// tracks the display-only variant in parallel: a tool that clamped its
	// output sets Result.Full, and the UI gets that while the model gets the
	// clamped Content. clamped stays false when no tool did, so the event
	// carries no redundant copy of the same string.
	var content, full string
	isErr := false
	clamped := false
	var images []tools.ResultImage
	for i, r := range results {
		if r.Child != nil {
			l.noteChild(tu.ID, r.Child)
		}
		if i > 0 && r.Content != "" {
			content += "\n"
			full += "\n"
		}
		content += r.Content
		full += r.Display()
		if r.Full != "" {
			clamped = true
		}
		isErr = isErr || r.IsError
		images = append(images, r.Images...)
	}

	// The backstop. Tools that understand their own output clamp it better
	// than this can — Bash keeps a tail because that is where the verdict is —
	// but most tools do not clamp at all, and before this every one of them
	// could put its entire output into the window. A 980 KB Grep result or a
	// chatty MCP server was one call away from an unusable session. Content
	// already within budget comes back untouched, so self-clamping tools are
	// unaffected.
	if capped, cut := tools.Cap(tu.Name, content); cut {
		content = capped
		clamped = true
	}

	// PostToolUse. After the cap, so a hook's feedback cannot be the part that
	// gets truncated away, and after the result is final so a hook reading the
	// file on disk sees what the model will be told about.
	//
	// A hook here can also refuse, which does not undo anything — the tool has
	// already run. What it buys is the result arriving as an error with the
	// reason attached, which is the difference between the model believing its
	// write succeeded and knowing the linter rejected it.
	if hookEnabled(opts, hooks.PostToolUse) {
		hr := fireHooks(ctx, opts, emit, hooks.Input{
			Event:      hooks.PostToolUse,
			ToolName:   tu.Name,
			ToolInput:  toolInputFor(raw),
			ToolResult: content,
			ToolError:  isErr,
		})
		if hr.Blocked {
			isErr = true
			content = hookFeedback(content, hr.Reason)
		} else {
			content = hookFeedback(content, hr.Context)
		}
	}
	if isErr {
		fs.fail(key)
		fs.bumpStreak(tu.Name, content, rawStr, false)
	} else {
		// A clean run resets both counters: this exact call's retry count and
		// the per-tool same-shape streak (the tool clearly isn't fundamentally
		// broken for the caller).
		fs.clear(key, tu.Name)
	}
	if emit != nil {
		ev := Event{Type: "tool_result", ToolName: tu.Name, ToolUseID: tu.ID, Content: content, IsError: isErr}
		if clamped {
			ev.FullContent = full
		}
		emit(ev)
	}
	if len(images) == 0 {
		return anthropic.NewBetaToolResultBlock(tu.ID, content, isErr)
	}
	return toolResultWithImages(tu.ID, content, isErr, images)
}

// userMessageWithImages builds a user message carrying text plus any base64
// image blocks (a TUI "@image.png" reference). Same block shape as an image
// tool_result, but on a user turn rather than a tool result. With no images it
// is exactly the plain-text message it replaces.
func userMessageWithImages(text string, images []tools.ResultImage) anthropic.BetaMessageParam {
	if len(images) == 0 {
		return anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text))
	}
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(images)+1)
	if text != "" {
		blocks = append(blocks, anthropic.NewBetaTextBlock(text))
	}
	for _, img := range images {
		blocks = append(blocks, anthropic.BetaContentBlockParamUnion{
			OfImage: &anthropic.BetaImageBlockParam{
				Source: anthropic.BetaImageBlockParamSourceUnion{
					OfBase64: &anthropic.BetaBase64ImageSourceParam{
						Data:      img.Base64,
						MediaType: anthropic.BetaBase64ImageSourceMediaType(img.MediaType),
					},
				},
			},
		})
	}
	return anthropic.NewBetaUserMessage(blocks...)
}

// toolResultWithImages builds a tool_result block carrying text plus one or
// more base64 image blocks (for Read of image files).
func toolResultWithImages(toolUseID, text string, isErr bool, images []tools.ResultImage) anthropic.BetaContentBlockParamUnion {
	content := make([]anthropic.BetaToolResultBlockParamContentUnion, 0, len(images)+1)
	if text != "" {
		content = append(content, anthropic.BetaToolResultBlockParamContentUnion{
			OfText: &anthropic.BetaTextBlockParam{Text: text},
		})
	}
	for _, img := range images {
		content = append(content, anthropic.BetaToolResultBlockParamContentUnion{
			OfImage: &anthropic.BetaImageBlockParam{
				Source: anthropic.BetaImageBlockParamSourceUnion{
					OfBase64: &anthropic.BetaBase64ImageSourceParam{
						Data:      img.Base64,
						MediaType: anthropic.BetaBase64ImageSourceMediaType(img.MediaType),
					},
				},
			},
		})
	}
	return anthropic.BetaContentBlockParamUnion{
		OfToolResult: &anthropic.BetaToolResultBlockParam{
			ToolUseID: toolUseID,
			IsError:   anthropic.Bool(isErr),
			Content:   content,
		},
	}
}

// buildToolParams converts the registry's tools into API tool params. Deferred
// tools are omitted unless they've been revealed (ToolSearch); ToolSearch itself
// is always included so the model can discover the deferred ones.
func (l *Loop) buildToolParams(ctx context.Context, deferred, revealed map[string]bool) ([]anthropic.BetaToolUnionParam, error) {
	names := l.tools.Names()
	out := make([]anthropic.BetaToolUnionParam, 0, len(names))
	for _, name := range names {
		if deferred[name] && !revealed[name] && name != "ToolSearch" {
			continue
		}
		t, _ := l.tools.Lookup(name)
		desc, err := t.Description(ctx)
		if err != nil {
			return nil, fmt.Errorf("tool %s description: %w", name, err)
		}
		props, required := splitSchema(t.InputSchema())
		out = append(out, anthropic.BetaToolUnionParam{
			OfTool: &anthropic.BetaToolParam{
				Name:        t.Name(),
				Description: anthropic.String(desc),
				InputSchema: anthropic.BetaToolInputSchemaParam{
					Properties: props,
					Required:   required,
				},
			},
		})
	}
	return out, nil
}

// splitSchema pulls "properties" and "required" out of a generated JSON Schema
// object so they can be placed into BetaToolInputSchemaParam.
// schemaFieldList renders a tool's accepted input fields as
// "file_path (required), offset, limit" — appended to validation errors so the
// model sees what's correct, not just what was wrong. Returns "" when the
// schema has no properties (e.g. parameterless tools).
func schemaFieldList(raw json.RawMessage) string {
	props, required := splitSchema(raw)
	pm, ok := props.(map[string]any)
	if !ok || len(pm) == 0 {
		return ""
	}
	req := make(map[string]bool, len(required))
	for _, r := range required {
		req[r] = true
	}
	names := make([]string, 0, len(pm))
	for k := range pm {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		if req[n] {
			parts[i] = n + " (required)"
		} else {
			parts[i] = n
		}
	}
	return strings.Join(parts, ", ")
}

func splitSchema(raw json.RawMessage) (properties any, required []string) {
	var s struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
		Defs       map[string]any  `json:"$defs"`
	}
	_ = json.Unmarshal(raw, &s)
	if len(s.Properties) > 0 {
		props := s.Properties
		// BetaToolInputSchemaParam carries only properties and required, so a
		// $ref whose target lived in $defs would be sent dangling. grok rejects
		// the whole request ("invalid request") for one such tool. Inline the
		// definitions first; a ref with no local target becomes an empty object
		// rather than a ref the request can no longer resolve. The schema
		// package already inlines its own schemas.
		if len(s.Defs) > 0 || strings.Contains(string(props), `"$ref"`) {
			props = inlineRefs(props, s.Defs)
		}
		var p any
		_ = json.Unmarshal(props, &p)
		properties = p
	}
	return properties, s.Required
}

// inlineRefs replaces {"$ref":"#/$defs/Name"} with the named definition.
// A cycle, or a ref that points anywhere else, becomes an empty object schema
// rather than a ref the request can no longer resolve.
func inlineRefs(raw json.RawMessage, defs map[string]any) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(inlineRefsValue(v, defs, nil))
	if err != nil {
		return raw
	}
	return out
}

func inlineRefsValue(v any, defs map[string]any, seen map[string]bool) any {
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			const prefix = "#/$defs/"
			name := strings.TrimPrefix(ref, prefix)
			def, known := defs[name]
			if !strings.HasPrefix(ref, prefix) || !known || seen[name] {
				return map[string]any{"type": "object"}
			}
			next := make(map[string]bool, len(seen)+1)
			for k := range seen {
				next[k] = true
			}
			next[name] = true
			return inlineRefsValue(def, defs, next)
		}
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = inlineRefsValue(val, defs, seen)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = inlineRefsValue(val, defs, seen)
		}
		return out
	default:
		return v
	}
}

// record marshals a message param and hands it to the recorder, returning
// the recorder's error.
func record(r Recorder, role string, msg any) error {
	if r == nil {
		return nil
	}
	// A response BetaMessage must be converted to its request param before it
	// is marshalled. The SDK's *response* union types carry no MarshalJSON, so
	// json.Marshal on one dumps every internal field of the union — a text
	// block comes out with "OfBetaWebSearchResultBlockArray" and a dozen other
	// empty fields alongside the real one. For plain text and tool_use that is
	// merely ugly, but for a server-tool result (web_search / web_fetch) the
	// content field serialises to an object the API rejects with
	// "content ... Input should be a valid array" the next time the history is
	// sent — which is every resume. ToParam() runs through the param
	// marshallers, which is clean and round-trips. RawJSON() is not an
	// alternative: after streaming accumulation the SDK sets it to this same
	// leaky marshal, so it is corrupt too. Params (user and tool_result
	// messages) already marshal correctly and pass straight through.
	if m, ok := msg.(anthropic.BetaMessage); ok {
		msg = m.ToParam()
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return r.Record(role, b)
}

// toolUseBlocks returns the tool_use blocks in an assistant message.
func toolUseBlocks(m anthropic.BetaMessage) []anthropic.BetaToolUseBlock {
	var out []anthropic.BetaToolUseBlock
	for _, b := range m.Content {
		if b.Type == "tool_use" {
			out = append(out, b.AsToolUse())
		}
	}
	return out
}

// finalAssistantText concatenates the text blocks of an assistant message.
func finalAssistantText(m anthropic.BetaMessage) string {
	var s string
	for _, b := range m.Content {
		if b.Type == "text" {
			s += b.AsText().Text
		}
	}
	return s
}

func (l *Loop) foldDelivered(opts Options, res *Result) {
	if opts.CollectChildUsage == nil {
		return
	}
	for i, u := range opts.CollectChildUsage() {
		l.noteChild(fmt.Sprintf("bg-%d", i), u)
	}
	l.foldChildren(opts, res)
}
