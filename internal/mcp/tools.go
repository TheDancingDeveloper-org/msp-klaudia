package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/textsafe"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// mcpPermission is the intrinsic decision for MCP tools.
//
// When the trust model is enforcing, MCP calls proceed without a prompt. The
// host gate runs in front of every tool call and is what actually protects the
// machine; asking again here made MCP the one door where the zone model was
// ignored and the user was handed a per-tool consent decision instead. That
// decision was not answerable in any useful way — "may godot_game_time run?"
// carries no information the user has — and it did not even accumulate, because
// approvals key on the qualified name: renaming a server, or reaching for the
// twenty-second tool on it, started again from nothing.
//
// What this gives up is real and worth stating. The gate classifies tool calls
// by reading their inputs, and it has no model of what an MCP server does, so
// an MCP call raises no concerns and is allowed. Trusting the zone model here
// means trusting the servers in .mcp.json roughly as much as the shell —
// which is the same bet as running them at all.
//
// A server you want available without that bet can be marked read-only in
// .mcp.json and reached from a read-only sub-agent; that path is governed by
// the readOnlyHint annotation rather than by the mode.
func mcpPermission(pctx permission.Context) permission.Decision {
	if permission.CurrentMode(pctx) == permission.ModePlan {
		// Plan mode is read-only for every tool, trusted or not.
		return permission.Decision{Behavior: permission.Deny, Message: "plan mode is read-only; MCP tools are not allowed"}
	}
	return permission.Decision{Behavior: permission.Allow}
}

// errTimedOut marks a call that ran out of its own per-call budget, as
// distinct from the turn being cancelled.
var errTimedOut = errors.New("mcp call timed out")

// mcpTool adapts a single MCP server tool to the Klaudia Tool interface. Its
// name is namespaced "mcp__<server>__<tool>" to avoid collisions.
type mcpTool struct {
	qualifiedName string
	remoteName    string
	description   string
	inputSchema   json.RawMessage
	server        *Server
	readOnly      bool
	timeout       time.Duration
	// mgr is held so a call can relaunch its own server; see Execute.
	mgr *Manager
}

func (t *mcpTool) Name() string                                { return t.qualifiedName }
func (t *mcpTool) Description(context.Context) (string, error) { return t.description, nil }
func (t *mcpTool) InputSchema() json.RawMessage                { return t.inputSchema }

// ReadOnly reports that this tool only reads, as declared by the server's
// readOnlyHint annotation or as overridden per server in .mcp.json.
//
// It exists so the read-only sub-agents can be given MCP tools without being
// given the ability to write. A tool that says nothing is not read-only: the
// annotation is optional in the protocol, and the safe reading of silence is
// that the author never thought about it.
//
// This governs which tools a read-only sub-agent is handed. It is not a claim
// that calling the tool is safe — nothing here verifies what a server does, and
// under an enforcing trust posture the main agent calls MCP tools without
// asking, read-only or not.
func (t *mcpTool) ReadOnly() bool { return t.readOnly }

// ValidateInput is a no-op beyond JSON well-formedness; the server validates
// against its own schema.
func (t *mcpTool) ValidateInput(raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("input is not valid JSON")
	}
	return nil
}

func (t *mcpTool) PermissionRequest(json.RawMessage) permission.PermissionRequest {
	return permission.PermissionRequest{Specifier: t.qualifiedName}
}

func (t *mcpTool) CheckPermissions(pctx permission.Context, _ permission.PermissionRequest) permission.Decision {
	return mcpPermission(pctx)
}

func (t *mcpTool) Execute(ctx context.Context, _ tools.Context, raw json.RawMessage) ([]tools.Result, error) {
	var args any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &args)
	}
	sess := t.server.sess()
	if sess == nil {
		// A server that failed to launch at startup, or was disconnected.
		// Worth one attempt before telling the model it is unusable: the
		// common cause is transient, and the alternative is a dead tool for
		// the rest of the session unless the user notices and runs /mcp.
		if !t.revive(ctx) {
			return []tools.Result{{Content: fmt.Sprintf("MCP server %q is disconnected and could not be restarted; reconnect it with /mcp.", t.server.Name), IsError: true}}, nil
		}
		sess = t.server.sess()
	}
	// Bounded: a server that stops answering mid-call (a dropped HTTP or SSE
	// connection) used to hold the turn until the user interrupted it, and a
	// headless run forever.
	timeout := t.timeout
	if timeout <= 0 {
		timeout = toolTimeout(ServerConfig{})
	}
	call := func(sess *mcpsdk.ClientSession) (*mcpsdk.CallToolResult, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := sess.CallTool(cctx, &mcpsdk.CallToolParams{Name: t.remoteName, Arguments: args})
		if err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, errTimedOut
		}
		return res, err
	}
	res, err := call(sess)
	if err == nil {
		// Capped like Bash output: a server can return megabytes in one call.
		return []tools.Result{tools.CapResult(resultOf(res))}, nil
	}
	if errors.Is(err, errTimedOut) {
		return []tools.Result{{Content: fmt.Sprintf("MCP call to %s timed out after %s — the server may be stuck; /mcp can reconnect it. Set \"timeout\" (seconds) on the server in .mcp.json, or KLAUDIA_MCP_TOOL_TIMEOUT, if the tool is just slow.", t.qualifiedName, timeout), IsError: true}}, nil
	}

	// The call failed on the wire. revive probes liveness first, so a bad
	// argument or an unknown tool — which also arrive as errors — cannot
	// restart a healthy server.
	if ctx.Err() != nil || !t.revive(ctx) {
		return []tools.Result{{Content: fmt.Sprintf("MCP call failed: %v", err), IsError: true}}, nil
	}

	// The server is back. Whether to run the call again is not a performance
	// question, it is a correctness one: the failure may have happened on the
	// way back from a call that already ran, and nothing on the wire
	// distinguishes that from one that never arrived. A read can be repeated
	// at worst wastefully; anything else is reported, with the ambiguity
	// stated, so the model checks rather than silently doing it twice.
	//
	// One case is not ambiguous: the server answering that it does not know
	// the session means it never ran the call, so that call is re-sent
	// whatever the tool does. A closed connection is NOT that case — it can
	// close after the server has already run the call (upstream's
	// TestAMutatingToolCallIsNotRepeatedAfterARevive).
	notSent := errors.Is(err, mcpsdk.ErrSessionMissing)
	if !t.readOnly && !notSent {
		return []tools.Result{{Content: fmt.Sprintf(
			"MCP call failed: %v. Server %q had stopped responding and has been restarted. "+
				"The call may or may not have taken effect before it died — check the state before retrying.",
			err, t.server.Name), IsError: true}}, nil
	}
	sess = t.server.sess()
	if sess == nil {
		return []tools.Result{{Content: fmt.Sprintf("MCP call failed: %v", err), IsError: true}}, nil
	}
	res, err = call(sess)
	if err != nil {
		return []tools.Result{{Content: fmt.Sprintf("MCP call failed after restarting server %q: %v", t.server.Name, err), IsError: true}}, nil
	}
	return []tools.Result{tools.CapResult(resultOf(res))}, nil
}

// revive relaunches the tool's server if it has died. It is a no-op returning
// false when the tool was built without a Manager (tests that wire a Server
// directly), so the old behaviour stands there.
func (t *mcpTool) revive(ctx context.Context) bool {
	if t.mgr == nil {
		return false
	}
	return t.mgr.revive(ctx, t.server)
}

// Tools lists every connected server's tools and wraps them. Servers that fail
// to list are skipped.
func (m *Manager) Tools(ctx context.Context) []tools.Tool {
	var out []tools.Tool
	for _, srv := range m.Servers() {
		sess := srv.sess()
		if sess == nil {
			continue
		}
		list, err := listTools(ctx, sess)
		srv.setListError(err)
		if err != nil {
			continue
		}
		// An override, when present, decides for every tool on the server and
		// the annotations are not consulted at all — in either direction.
		cfg, _ := m.serverConfig(srv.Name)
		override := cfg.ReadOnly
		for _, rt := range list {
			schema, _ := json.Marshal(rt.InputSchema)
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			readOnly := rt.Annotations != nil && rt.Annotations.ReadOnlyHint
			if override != nil {
				readOnly = *override
			}
			out = append(out, &mcpTool{
				qualifiedName: fmt.Sprintf("mcp__%s__%s", srv.Name, rt.Name),
				remoteName:    rt.Name,
				description:   toolDescription(rt.Description),
				inputSchema:   schema,
				server:        srv,
				readOnly:      readOnly,
				timeout:       toolTimeout(cfg),
				mgr:           m,
			})
		}
	}
	return out
}

// ResourceTools returns the ListMcpResources and ReadMcpResource tools backed
// by this manager.
func (m *Manager) ResourceTools() ([]tools.Tool, error) {
	list, err := newListResourcesTool(m)
	if err != nil {
		return nil, err
	}
	read, err := newReadResourceTool(m)
	if err != nil {
		return nil, err
	}
	return []tools.Tool{list, read}, nil
}

// findServer returns the connected server with the given name.
func (m *Manager) findServer(name string) *Server { return m.find(name) }

// formatResourceList renders the resources of all servers as text.
func (m *Manager) formatResourceList(ctx context.Context) string {
	var b strings.Builder
	for _, srv := range m.Servers() {
		sess := srv.sess()
		if sess == nil {
			continue
		}
		res, err := sess.ListResources(ctx, &mcpsdk.ListResourcesParams{})
		if err != nil {
			continue
		}
		for _, r := range res.Resources {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", srv.Name, r.URI, r.Name)
		}
	}
	if b.Len() == 0 {
		return "No MCP resources available."
	}
	return strings.TrimRight(b.String(), "\n")
}

// listTools reads a server's whole tool list, every page of it, retrying once
// after a short pause when the read fails.
//
// One ListTools call read the first page only, so a server that paginates lost
// every tool past it; and a failure was skipped with `continue`, which looked
// exactly like a server with no tools.
func listTools(ctx context.Context, sess *mcpsdk.ClientSession) ([]*mcpsdk.Tool, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(listRetryDelay):
			}
		}
		var list []*mcpsdk.Tool
		err = nil
		for tool, terr := range sess.Tools(ctx, nil) {
			if terr != nil {
				err = terr
				break
			}
			list = append(list, tool)
		}
		if err == nil {
			return list, nil
		}
	}
	return nil, fmt.Errorf("listing tools: %w", err)
}

// listRetryDelay is the pause before a failed tools/list is tried again.
var listRetryDelay = 500 * time.Millisecond

// maxToolDescription caps an MCP tool's description. A server writes it and
// every request carries it: an uncapped one costs context on every turn, and
// is the easiest place for a server to put instructions for the model.
const maxToolDescription = 2048

// toolDescription is a server's tool description with invisible characters
// removed and its length capped.
func toolDescription(d string) string {
	return textsafe.Truncate(textsafe.StripInvisible(d), maxToolDescription, " […description truncated]")
}
