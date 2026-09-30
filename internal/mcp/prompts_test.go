package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// startPromptServer connects a Manager to an in-process server exposing two
// prompts: "greet" (one required argument) and "ping" (no arguments).
func startPromptServer(t *testing.T) (*Manager, func()) {
	t.Helper()
	ctx := context.Background()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "psrv", Version: "0.0.1"}, nil)
	srv.AddPrompt(&mcpsdk.Prompt{
		Name:        "greet",
		Description: "Greet someone by name",
		Arguments:   []*mcpsdk.PromptArgument{{Name: "name", Description: "who to greet", Required: true}},
	}, func(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
		who := req.Params.Arguments["name"]
		return &mcpsdk.GetPromptResult{
			Description: "a greeting",
			Messages: []*mcpsdk.PromptMessage{{
				Role:    "user",
				Content: &mcpsdk.TextContent{Text: "Hello, " + who + "!"},
			}},
		}, nil
	})
	srv.AddPrompt(&mcpsdk.Prompt{Name: "ping", Description: "no args"},
		func(_ context.Context, _ *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
			return &mcpsdk.GetPromptResult{
				Messages: []*mcpsdk.PromptMessage{{Role: "user", Content: &mcpsdk.TextContent{Text: "pong"}}},
			}, nil
		})

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client, err := ConnectTransport(ctx, "psrv", clientT)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	m := &Manager{}
	m.Add(client)
	return m, func() { m.Close(); _ = ss.Wait() }
}

func TestPromptsListReportsServerPrompts(t *testing.T) {
	m, cleanup := startPromptServer(t)
	defer cleanup()

	prompts := m.Prompts(context.Background())
	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2: %+v", len(prompts), prompts)
	}
	// Sorted by name: "greet" before "ping".
	greet := prompts[0]
	if greet.Name != "greet" || greet.Qualified != "mcp__psrv__greet" {
		t.Errorf("greet = %+v", greet)
	}
	if len(greet.Arguments) != 1 || greet.Arguments[0].Name != "name" || !greet.Arguments[0].Required {
		t.Errorf("greet arguments = %+v, want one required 'name'", greet.Arguments)
	}
}

func TestGetPromptRendersTemplatedText(t *testing.T) {
	m, cleanup := startPromptServer(t)
	defer cleanup()

	text, err := m.GetPrompt(context.Background(), "psrv", "greet", map[string]string{"name": "World"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if text != "Hello, World!" {
		t.Errorf("rendered = %q, want %q", text, "Hello, World!")
	}
}

func TestGetPromptUnknownServer(t *testing.T) {
	m, cleanup := startPromptServer(t)
	defer cleanup()

	if _, err := m.GetPrompt(context.Background(), "nope", "greet", nil); err == nil {
		t.Error("expected an error for an unknown server")
	}
}

func TestParsePromptArgsFillsFirstArgFromBareText(t *testing.T) {
	p := PromptInfo{Arguments: []PromptArgument{{Name: "name", Required: true}}}
	got := ParsePromptArgs(p, "the whole login flow")
	if got["name"] != "the whole login flow" {
		t.Errorf("bare text = %v, want it assigned to the required arg", got)
	}
}

func TestParsePromptArgsParsesKeyValue(t *testing.T) {
	p := PromptInfo{Arguments: []PromptArgument{{Name: "name"}, {Name: "lang"}}}
	got := ParsePromptArgs(p, "name=Ada lang=go")
	if got["name"] != "Ada" || got["lang"] != "go" {
		t.Errorf("key=value parse = %v", got)
	}
}

func TestParsePromptArgsPrefersRequiredForBareText(t *testing.T) {
	p := PromptInfo{Arguments: []PromptArgument{
		{Name: "optional"},
		{Name: "topic", Required: true},
	}}
	got := ParsePromptArgs(p, "databases")
	if got["topic"] != "databases" {
		t.Errorf("bare text = %v, want it on the required 'topic' arg", got)
	}
	if _, ok := got["optional"]; ok {
		t.Errorf("bare text should not fall on the optional arg: %v", got)
	}
}

func TestRenderPromptMessagesLabelsMixedRoles(t *testing.T) {
	out := renderPromptMessages([]*mcpsdk.PromptMessage{
		{Role: "user", Content: &mcpsdk.TextContent{Text: "hi"}},
		{Role: "assistant", Content: &mcpsdk.TextContent{Text: "hello"}},
	})
	if !strings.Contains(out, "user: hi") || !strings.Contains(out, "assistant: hello") {
		t.Errorf("mixed-role render = %q, want role-labelled lines", out)
	}
}
