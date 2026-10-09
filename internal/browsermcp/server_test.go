package browsermcp

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/greenthread-ai/klaudia/internal/browser"
)

// fakeEngine records calls and returns canned pages. It is the seam the
// suite uses instead of Chrome.
type fakeEngine struct {
	navigated []string
	snaps     [][2]bool
	searches  []browser.SearchOptions
	closed    bool

	page    *browser.Snapshot
	results []browser.SearchResult
	fail    error
}

func (f *fakeEngine) Navigate(u string) error {
	f.navigated = append(f.navigated, u)
	return f.fail
}

func (f *fakeEngine) Snapshot(html, md bool) (*browser.Snapshot, error) {
	f.snaps = append(f.snaps, [2]bool{html, md})
	if f.fail != nil {
		return nil, f.fail
	}
	if f.page == nil {
		return &browser.Snapshot{}, nil
	}
	return f.page, nil
}

func (f *fakeEngine) Search(_ context.Context, opts browser.SearchOptions) ([]browser.SearchResult, error) {
	f.searches = append(f.searches, opts)
	return f.results, f.fail
}

func (f *fakeEngine) Close() { f.closed = true }

func dial(t *testing.T, e Engine) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := NewServer(e)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, t1, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := c.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestToolsNavigateFetchSearchAndReject(t *testing.T) {
	e := &fakeEngine{
		page: &browser.Snapshot{URL: "https://example.com/landed", Title: "Landed", HTML: "<p>hi</p>", Markdown: "hi"},
		results: []browser.SearchResult{
			{Title: "One", URL: "https://example.com/1", Snippet: "s"},
		},
	}
	cs := dial(t, e)
	ctx := context.Background()

	names := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names[tool.Name] = true
	}
	for _, want := range []string{"browser_navigate", "browser_snapshot", "browser_fetch", "web_search"} {
		if !names[want] {
			t.Errorf("tool %s not advertised", want)
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "browser_navigate",
		Arguments: map[string]any{"url": "https://example.com"},
	})
	if err != nil || res.IsError {
		t.Fatalf("navigate: %v %+v", err, res)
	}
	if len(e.navigated) != 1 || e.navigated[0] != "https://example.com" {
		t.Fatalf("navigated %v", e.navigated)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "browser_snapshot",
		Arguments: map[string]any{"include_html": true},
	})
	if err != nil || res.IsError {
		t.Fatalf("snapshot: %v %+v", err, res)
	}
	if !e.snaps[len(e.snaps)-1][0] || e.snaps[len(e.snaps)-1][1] {
		t.Fatalf("snapshot flags %v", e.snaps)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "browser_fetch",
		Arguments: map[string]any{"url": "https://example.com/page"},
	})
	if err != nil || res.IsError {
		t.Fatalf("fetch: %v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "hi") {
		t.Fatalf("fetch content %q", text)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "web_search",
		Arguments: map[string]any{"query": "klaudia", "max_results": 3, "allowed_domains": []string{"example.com"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %+v", err, res)
	}
	if len(e.searches) != 1 || e.searches[0].Query != "klaudia" || e.searches[0].MaxResults != 3 {
		t.Fatalf("search opts %+v", e.searches)
	}

	for _, bad := range []string{"ftp://example.com", "example.com", "javascript:alert(1)", ""} {
		res, err = cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "browser_navigate",
			Arguments: map[string]any{"url": bad},
		})
		if err != nil {
			t.Fatalf("bad url %q: protocol error %v", bad, err)
		}
		if !res.IsError {
			t.Fatalf("bad url %q was accepted", bad)
		}
	}
}

func TestWebSearchRequiresQuery(t *testing.T) {
	cs := dial(t, &fakeEngine{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_search",
		Arguments: map[string]any{"query": "  "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("blank query accepted")
	}
}
