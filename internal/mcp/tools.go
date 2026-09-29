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
// which is the same bet as running them at all, and is why this follows the
// trust posture rather than being unconditional.
//
// Without trust enforcing, the old behaviour stands: ask in interactive modes,
// refuse where there is nobody to ask.
func mcpPermission(pctx permission.Context) permission.Decision {
	if permission.CurrentMode(pctx) == permission.ModePlan {
		// Plan mode is read-only for every tool, trusted or not.
		return permission.Decision{Behavior: permission.Deny, Message: "plan mode is read-only; MCP tools are not allowed"}
	}
	if permission.IsTrusting(pctx) {
		return permission.Decision{Behavior: permission.Allow}
	}
	if permission.CurrentMode(pctx) == permission.ModeDontAsk {
		return permission.Decision{Behavior: permission.Deny, Message: "not pre-approved (dontAsk mode)"}
	}
	return permission.Decision{Behavior: permission.Ask}
}

// mcpTool adapts a single MCP server tool to the Klaudia Tool interface. Its
// name is namespaced "mcp__<server>__<tool>" to avoid collisions.
type mcpTool struct {
	qualifiedName string
	remoteName    string
	description   string
	inputSchema   json.RawMessage
	server        *Server
	readOnly      bool
	// reconnect re-establishes the server's session (Manager.Reconnect).
	reconnect func() error
	timeout   time.Duration
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
		return []tools.Result{{Content: fmt.Sprintf("MCP server %q is disconnected; reconnect it with /mcp.", t.server.Name), IsError: true}}, nil
	}
	// Bounded: a server that stops answering mid-call (a dropped HTTP or SSE
	// connection) used to hold the turn until the user interrupted it, and a
	// headless run forever.
	timeout := t.timeout
	if timeout <= 0 {
		timeout = toolTimeout(ServerConfig{})
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	params := &mcpsdk.CallToolParams{Name: t.remoteName, Arguments: args}
	res, err := sess.CallTool(cctx, params)
	// A session that is gone - the connection closed, or a remote server
	// that restarted and no longer knows it - used to leave the server
	// "connected" and every call failing until someone ran /mcp. Neither
	// error means the call ran (it was not sent, or the server had no session
	// to run it in), so reconnect once and send it again.
	if err != nil && ctx.Err() == nil && t.reconnect != nil &&
		(errors.Is(err, mcpsdk.ErrConnectionClosed) || errors.Is(err, mcpsdk.ErrSessionMissing)) {
		if rerr := t.reconnect(); rerr == nil {
			if fresh := t.server.sess(); fresh != nil {
				res, err = fresh.CallTool(cctx, params)
			}
		}
	}
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return []tools.Result{{Content: fmt.Sprintf("MCP call to %s timed out after %s — the server may be stuck; /mcp can reconnect it. Set \"timeout\" (seconds) on the server in .mcp.json, or KLAUDIA_MCP_TOOL_TIMEOUT, if the tool is just slow.", t.qualifiedName, timeout), IsError: true}}, nil
		}
		return []tools.Result{{Content: fmt.Sprintf("MCP call failed: %v", err), IsError: true}}, nil
	}
	// Capped like Bash output: a server can return megabytes in one call.
	return []tools.Result{tools.CapResult(resultOf(res))}, nil
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
				reconnect:     func() error { return m.Reconnect(srv.Name) },
				timeout:       toolTimeout(cfg),
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
