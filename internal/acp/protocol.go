package acp

import "encoding/json"

// protocolVersion is the ACP major version this agent speaks.
//
// One, not two. ACP v2 exists as a published Draft and changes real things —
// tool_call folded into an upsert-only tool_call_update, a state_update for the
// turn lifecycle, agent-owned terminals — but v1 is the version every shipping
// client negotiates today. The version is agreed at initialize, so moving to v2
// later is additive: answer 2 when a client asks for 2, keep answering 1 for
// everyone else.
const protocolVersion = 1

// ---------------------------------------------------------------- initialize

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
}

// clientCapabilities is what the editor offers the agent.
//
// FS.ReadTextFile is acted on: Read goes through the client when it is set, so
// the model sees unsaved buffers. FS.WriteTextFile and Terminal are recorded and
// not used — both are deliberate decisions, and the reasoning is in the package
// doc.
type clientCapabilities struct {
	FS       fsCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AuthMethods       []authMethod      `json:"authMethods"`
}

type agentCapabilities struct {
	LoadSession         bool                 `json:"loadSession"`
	PromptCapabilities  promptCapabilities   `json:"promptCapabilities"`
	MCPCapabilities     mcpCapabilities      `json:"mcpCapabilities"`
	SessionCapabilities *sessionCapabilities `json:"sessionCapabilities,omitempty"`
}

// sessionCapabilities declares the optional session methods the agent serves.
// Each one is a *present or absent* object rather than a boolean, so an empty
// struct is the whole payload: "list is supported" is `"list": {}`.
//
// Klaudia advertises list and close. delete is left out on purpose — a
// transcript is the user's record of what the agent did on their machine, and
// an editor's "close tab" gesture should not be able to erase it. resume is
// session/load under another name. additionalDirectories is left out because
// the extra directories Klaudia has (the TUI's /add-dir) are informational
// prompt context, not a second root the tools work in: accepting them here
// would promise an editor that a folder it added to the workspace is one
// Klaudia will edit in, which is not what they do.
type sessionCapabilities struct {
	List  *sessionMethodCapability `json:"list,omitempty"`
	Close *sessionMethodCapability `json:"close,omitempty"`
}

type sessionMethodCapability struct{}

// promptCapabilities declares which content block types may appear in a
// session/prompt.
//
// Image and audio are false because the agent loop takes a string prompt: there
// is nowhere for the bytes to go, and claiming the capability would have the
// editor send a screenshot that Klaudia silently discarded. embeddedContext is
// true — a resource block carrying text is inlined into the prompt, which is
// the whole point of an editor pasting the open buffer in.
type promptCapabilities struct {
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
	Image           bool `json:"image"`
}

// mcpCapabilities declares which MCP transports the agent can connect on behalf
// of the client. Both false: Klaudia connects the servers in .mcp.json itself
// and does not take a server list from the editor (see newSession).
type mcpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type authMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ------------------------------------------------------------------ sessions

type newSessionParams struct {
	CWD string `json:"cwd"`
	// MCPServers is the editor's own MCP server list. Parsed only so that a
	// non-empty one can be reported rather than ignored in silence.
	MCPServers []json.RawMessage `json:"mcpServers,omitempty"`
}

type newSessionResult struct {
	SessionID string            `json:"sessionId"`
	Modes     *sessionModeState `json:"modes,omitempty"`
}

// loadSessionParams reopens a conversation Klaudia persisted earlier. The id is
// the client's, from a previous session/new or session/list, which is why ACP
// session ids are Klaudia transcript ids and not a second numbering.
type loadSessionParams struct {
	SessionID  string            `json:"sessionId"`
	CWD        string            `json:"cwd"`
	MCPServers []json.RawMessage `json:"mcpServers,omitempty"`
}

// loadSessionResult carries only the mode state: the conversation itself is
// replayed as session/update notifications before this is returned.
type loadSessionResult struct {
	Modes *sessionModeState `json:"modes,omitempty"`
}

type listSessionsParams struct {
	// CWD is optional and nullable. Klaudia serves one project per process, so
	// a request for a different one comes back empty rather than wrong.
	CWD    *string `json:"cwd,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

type listSessionsResult struct {
	Sessions []sessionInfo `json:"sessions"`
	// NextCursor is always null: Klaudia lists one project's transcripts, which
	// is tens of entries, and paginating that would be ceremony.
	NextCursor *string `json:"nextCursor"`
}

type sessionInfo struct {
	SessionID string  `json:"sessionId"`
	CWD       string  `json:"cwd"`
	Title     *string `json:"title,omitempty"`
	UpdatedAt *string `json:"updatedAt,omitempty"`
}

type closeSessionParams struct {
	SessionID string `json:"sessionId"`
}

type sessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []sessionMode `json:"availableModes"`
}

type sessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type setModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

// ACP v1 stop reasons. Klaudia's own vocabulary is wider (user_halt,
// blocked_by_hook, model_context_window_exceeded, …); stopReasonFor maps it.
const (
	stopEndTurn         = "end_turn"
	stopMaxTokens       = "max_tokens"
	stopMaxTurnRequests = "max_turn_requests"
	stopRefusal         = "refusal"
	stopCancelled       = "cancelled"
)

// ------------------------------------------------------------- content blocks

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// image / audio
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	// resource_link
	URI  string `json:"uri,omitempty"`
	Name string `json:"name,omitempty"`
	// resource (embedded context)
	Resource *embeddedResource `json:"resource,omitempty"`
}

type embeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

func textBlock(s string) contentBlock { return contentBlock{Type: "text", Text: s} }

// ------------------------------------------------------------ session updates

// sessionNotification is the envelope of every session/update.
type sessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// The session/update variants Klaudia emits. They are separate types rather
// than one struct with a discriminator field because "content" is a single
// ContentBlock on a message chunk and a ToolCallContent array on a tool call —
// the same JSON name with two incompatible shapes, which one Go struct cannot
// carry.

type messageChunk struct {
	Kind    string       `json:"sessionUpdate"`
	Content contentBlock `json:"content"`
}

func agentMessage(text string) messageChunk {
	return messageChunk{Kind: "agent_message_chunk", Content: textBlock(text)}
}

// agentThought carries Klaudia's own diagnostics — compaction, hook notices,
// dropped server-tool warnings.
//
// ACP v1 has no channel for "something happened that the user should know about
// but the model did not say". A thought chunk is the closest fit and, more to
// the point, the one clients render as dimmed, collapsible, out-of-band text,
// which is exactly how a notice should look. Putting them in agent_message_chunk
// would splice CLI diagnostics into the model's prose.
func agentThought(text string) messageChunk {
	return messageChunk{Kind: "agent_thought_chunk", Content: textBlock(text)}
}

// userMessage echoes the user's own turn back to the client. Only session/load
// sends these: during a live prompt the client already knows what it typed, but
// a replayed conversation is one it has never seen.
func userMessage(text string) messageChunk {
	return messageChunk{Kind: "user_message_chunk", Content: textBlock(text)}
}

type toolCallStart struct {
	Kind       string             `json:"sessionUpdate"`
	ToolCallID string             `json:"toolCallId"`
	Name       string             `json:"name,omitempty"`
	Title      string             `json:"title"`
	ToolKind   string             `json:"kind,omitempty"`
	Status     string             `json:"status,omitempty"`
	Content    []toolCallContent  `json:"content,omitempty"`
	Locations  []toolCallLocation `json:"locations,omitempty"`
	RawInput   any                `json:"rawInput,omitempty"`
}

type toolCallPatch struct {
	Kind       string            `json:"sessionUpdate"`
	ToolCallID string            `json:"toolCallId"`
	Status     string            `json:"status,omitempty"`
	Title      string            `json:"title,omitempty"`
	Content    []toolCallContent `json:"content,omitempty"`
}

type toolCallContent struct {
	Type    string        `json:"type"`
	Content *contentBlock `json:"content,omitempty"`
	// The "diff" variant. ACP flattens Diff's fields onto the content object
	// rather than nesting them under a "diff" key, so they live here.
	//
	// Both texts are pointers, and omitempty on a pointer tests the pointer
	// rather than the string, which is the property being used: an empty
	// oldText (the file exists and is empty) still serializes, while a nil one
	// (the file does not exist) is absent, and a client reads that as a new
	// file. A plain string would collapse those two into one. Same for
	// NewText, where truncating a file to nothing is a real edit the spec
	// requires the field for.
	Path    string  `json:"path,omitempty"`
	OldText *string `json:"oldText,omitempty"`
	NewText *string `json:"newText,omitempty"`
}

func toolText(s string) toolCallContent {
	b := textBlock(s)
	return toolCallContent{Type: "content", Content: &b}
}

// toolDiff is the content block a client renders as a diff widget instead of as
// a wall of text. old is nil for a file being created.
func toolDiff(path string, old *string, new string) toolCallContent {
	return toolCallContent{Type: "diff", Path: path, OldText: old, NewText: &new}
}

type toolCallLocation struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

type modeUpdate struct {
	Kind          string `json:"sessionUpdate"`
	CurrentModeID string `json:"currentModeId"`
}

// planUpdate is the agent's task list. It replaces the whole plan each time —
// ACP has no incremental form — which matches TodoWrite, whose contract is also
// "send the complete list every call".
type planUpdate struct {
	Kind    string      `json:"sessionUpdate"`
	Entries []planEntry `json:"entries"`
}

type planEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// Plan entry statuses and priorities. The statuses are the same three words
// TodoWrite uses, so the mapping is identity; priority has no Klaudia
// equivalent and the field is required, so every entry is medium.
const (
	planPending    = "pending"
	planInProgress = "in_progress"
	planCompleted  = "completed"

	planPriorityMedium = "medium"
)

// availableCommandsUpdate tells the client what it may offer as a /command.
type availableCommandsUpdate struct {
	Kind     string             `json:"sessionUpdate"`
	Commands []availableCommand `json:"availableCommands"`
}

type availableCommand struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Input       *commandInput `json:"input,omitempty"`
}

// commandInput is ACP's "unstructured" input variant: whatever the user typed
// after the command name arrives as one string. It is the only variant v1
// defines, and it is the right one — a skill substitutes $ARGUMENTS, which is
// free text.
type commandInput struct {
	Hint string `json:"hint"`
}

// usageUpdate reports context pressure: tokens resident in the conversation
// against the model's window. A client renders it as the gauge Klaudia's own
// status line shows.
type usageUpdate struct {
	Kind string `json:"sessionUpdate"`
	Used int    `json:"used"`
	Size int    `json:"size"`
}

// ACP tool kinds and statuses.
const (
	kindRead       = "read"
	kindEdit       = "edit"
	kindSearch     = "search"
	kindExecute    = "execute"
	kindFetch      = "fetch"
	kindSwitchMode = "switch_mode"
	kindOther      = "other"

	statusPending    = "pending"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusFailed     = "failed"
)

// ------------------------------------------------------- permission requests

type requestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  toolCallStart      `json:"toolCall"`
	Options   []permissionOption `json:"options"`
	Meta      map[string]any     `json:"_meta,omitempty"`
}

type permissionOption struct {
	OptionID string         `json:"optionId"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// Permission option kinds. Klaudia only ever offers the "once" pair: an
// always-allow would have to be remembered somewhere, and the place Klaudia
// remembers standing decisions is the trust store, which the user manages with
// /trust rather than by clicking a button in an editor dialog.
const (
	optAllowOnce  = "allow_once"
	optRejectOnce = "reject_once"
)

type requestPermissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected" | "cancelled"
	OptionID string `json:"optionId,omitempty"`
}

// ------------------------------------------------------------------ filesystem

// readTextFileParams asks the client for a file. Line is 1-based and Limit
// counts lines; both are pointers because omitting them means "the whole file",
// which zero does not.
type readTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

type readTextFileResult struct {
	Content string `json:"content"`
}
