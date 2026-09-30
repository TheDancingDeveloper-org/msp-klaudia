package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// PromptArgument describes one argument a server prompt accepts.
type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// PromptInfo is one server prompt, flattened for discovery and for exposing as
// a slash command. Qualified is the "mcp__<server>__<name>" form that mirrors
// how MCP tools are namespaced, so a prompt and a tool never collide and both
// read the same way to the user.
type PromptInfo struct {
	Server      string
	Name        string
	Qualified   string
	Title       string
	Description string
	Arguments   []PromptArgument
}

// Prompts lists the prompts of every connected server, in a stable order
// (server then prompt name). A server that does not implement prompts, or fails
// to list them, contributes nothing rather than failing the whole call —
// exactly how Tools treats a server it cannot list.
func (m *Manager) Prompts(ctx context.Context) []PromptInfo {
	var out []PromptInfo
	for _, srv := range m.Servers() {
		sess := srv.sess()
		if sess == nil {
			continue
		}
		res, err := sess.ListPrompts(ctx, &mcpsdk.ListPromptsParams{})
		if err != nil {
			continue
		}
		for _, p := range res.Prompts {
			if p == nil {
				continue
			}
			args := make([]PromptArgument, 0, len(p.Arguments))
			for _, a := range p.Arguments {
				if a == nil {
					continue
				}
				args = append(args, PromptArgument{
					Name:        a.Name,
					Description: a.Description,
					Required:    a.Required,
				})
			}
			out = append(out, PromptInfo{
				Server:      srv.Name,
				Name:        p.Name,
				Qualified:   fmt.Sprintf("mcp__%s__%s", srv.Name, p.Name),
				Title:       p.Title,
				Description: p.Description,
				Arguments:   args,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// GetPrompt fetches a prompt from a server and renders its messages to a single
// string suitable for submitting as a turn prompt. args are the templating
// arguments (prompts/get "arguments"). A disconnected or unknown server, or a
// server-side error, is returned as an error.
func (m *Manager) GetPrompt(ctx context.Context, server, name string, args map[string]string) (string, error) {
	srv := m.find(server)
	if srv == nil {
		return "", fmt.Errorf("unknown MCP server %q", server)
	}
	sess := srv.sess()
	if sess == nil {
		return "", fmt.Errorf("MCP server %q is disconnected", server)
	}
	res, err := sess.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	return renderPromptMessages(res.Messages), nil
}

// renderPromptMessages flattens prompt messages into text. The text content of
// every message is concatenated in order; non-text content (images, embedded
// resources) is skipped, since the destination is a text turn prompt. When more
// than one role is present the messages are prefixed with their role, so a
// multi-turn prompt template does not collapse into an ambiguous blob.
func renderPromptMessages(msgs []*mcpsdk.PromptMessage) string {
	roles := map[mcpsdk.Role]bool{}
	for _, msg := range msgs {
		if msg != nil {
			roles[msg.Role] = true
		}
	}
	label := len(roles) > 1

	var b strings.Builder
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		text := ""
		if tc, ok := msg.Content.(*mcpsdk.TextContent); ok {
			text = tc.Text
		}
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if label {
			fmt.Fprintf(&b, "%s: %s", msg.Role, text)
		} else {
			b.WriteString(text)
		}
	}
	return b.String()
}

// ParsePromptArgs turns a raw slash-command argument string into named prompt
// arguments. Tokens of the form key=value set that argument; any remaining
// bare text (tokens with no '=') is joined and assigned to the prompt's first
// declared argument — the common "one free-text argument" case, so
// "/mcp__srv__review the login flow" works without key=value ceremony.
//
// It is deliberately forgiving: an unknown key is passed through (the server
// validates), and a prompt with no declared arguments simply gets whatever
// key=value pairs were typed.
func ParsePromptArgs(p PromptInfo, raw string) map[string]string {
	out := map[string]string{}
	var bare []string
	for _, tok := range strings.Fields(raw) {
		if k, v, ok := strings.Cut(tok, "="); ok && k != "" {
			out[k] = v
			continue
		}
		bare = append(bare, tok)
	}
	if len(bare) > 0 && len(p.Arguments) > 0 {
		// Prefer the first required argument; fall back to the first declared.
		target := p.Arguments[0].Name
		for _, a := range p.Arguments {
			if a.Required {
				target = a.Name
				break
			}
		}
		if _, taken := out[target]; !taken {
			out[target] = strings.Join(bare, " ")
		}
	}
	return out
}
