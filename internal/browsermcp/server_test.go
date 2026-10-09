package browsermcp

import (
	"context"
	"strings"
	"sync"
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

	// landed is the URL a navigation ends on. Empty means the URL asked for.
	landed string
	// hold is closed by the test to release a Navigate that has started,
	// so two calls can be arranged to overlap.
	hold chan struct{}
	mu   sync.Mutex
}

func (f *fakeEngine) Navigate(u string) error {
	f.mu.Lock()
	f.navigated = append(f.navigated, u)
	f.mu.Unlock()
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.landed != "" {
		f.page = &browser.Snapshot{URL: f.landed, Title: "landed"}
	}
	return f.fail
}

func (f *fakeEngine) Snapshot(html, md bool) (*browser.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	return dialOpts(t, e, ServerOpts{})
}

func dialOpts(t *testing.T, e Engine, opts ServerOpts) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := NewServer(e, opts)
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

func TestPrivateNetworksRefusedUnlessAllowed(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/",
		"http://127.9.9.9/",
		"http://[::1]/",
		"http://169.254.1.1/",
		"http://[fe80::1]/",
		"http://0.0.0.0/",
		"http://[::]/",
		"http://224.0.0.1/",
		"http://[ff02::1]/",
		"http://10.1.2.3/",
		"http://172.16.0.1/",
		"http://172.31.255.1/",
		"http://192.168.1.1/",
		"http://100.64.0.1/",
		"http://100.127.0.1/",
		"http://[fc00::1]/",
		"http://[fd12:3456::1]/",
		"http://[::ffff:10.1.2.3]/",
		"http://[::ffff:127.0.0.1]/",
		"http://[::ffff:169.254.1.1]/",
	}
	e := &fakeEngine{}
	cs := dial(t, e)
	ctx := context.Background()
	for _, u := range blocked {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "browser_navigate",
			Arguments: map[string]any{"url": u},
		})
		if err != nil {
			t.Fatalf("%s: protocol error %v", u, err)
		}
		if !res.IsError {
			t.Errorf("%s was navigated to", u)
		}
	}
	if len(e.navigated) != 0 {
		t.Fatalf("refused URLs still reached the engine: %v", e.navigated)
	}

	allowed := dialOpts(t, e, ServerOpts{AllowPrivate: true})
	res, err := allowed.CallTool(ctx, &mcp.CallToolParams{
		Name:      "browser_navigate",
		Arguments: map[string]any{"url": "http://192.168.1.1/"},
	})
	if err != nil || res.IsError {
		t.Fatalf("--allow-private still refused 192.168.1.1: %v %+v", err, res)
	}
}

func TestRedirectToPrivateIsRefused(t *testing.T) {
	e := &fakeEngine{landed: "http://10.0.0.5/secret"}
	cs := dial(t, e)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "browser_navigate",
		Arguments: map[string]any{"url": "https://example.com/go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a redirect onto 10.0.0.5 was returned")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "10.0.0.5") {
		t.Fatalf("error %q does not name the address Chrome landed on", text)
	}

	// The same page must not come back through a later snapshot either.
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "browser_snapshot"})
	if err != nil || !res.IsError {
		t.Fatalf("snapshot of the private page: %v %+v", err, res)
	}
}

func TestNavigateAndSnapshotDoNotInterleave(t *testing.T) {
	e := &fakeEngine{hold: make(chan struct{})}
	cs := dial(t, e)
	ctx := context.Background()

	type result struct {
		text string
		err  error
	}
	nav := make(chan result, 1)
	go func() {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "browser_navigate",
			Arguments: map[string]any{"url": "https://example.com/first"},
		})
		if err != nil || res.IsError {
			nav <- result{err: err}
			return
		}
		nav <- result{text: res.Content[0].(*mcp.TextContent).Text}
	}()

	// The navigation blocks inside Navigate, which it can only reach while
	// holding the handler lock. The snapshot requested then must wait for
	// that navigation's own snapshot, or it would describe the wrong page.
	for {
		e.mu.Lock()
		started := len(e.navigated) > 0
		e.mu.Unlock()
		if started {
			break
		}
	}
	snap := make(chan result, 1)
	go func() {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "browser_snapshot",
			Arguments: map[string]any{"include_markdown": true},
		})
		if err != nil || res.IsError {
			snap <- result{err: err}
			return
		}
		snap <- result{text: res.Content[0].(*mcp.TextContent).Text}
	}()

	e.page = &browser.Snapshot{URL: "https://example.com/first", Title: "First", Markdown: "from the navigation"}
	close(e.hold)

	got := <-nav
	if got.err != nil || !strings.Contains(got.text, "example.com/first") {
		t.Fatalf("navigate: %v %q", got.err, got.text)
	}
	got = <-snap
	if got.err != nil {
		t.Fatal(got.err)
	}
	if e.snaps[0] != [2]bool{false, false} || e.snaps[1] != [2]bool{false, true} {
		t.Fatalf("snapshots ran in the wrong order: %v", e.snaps)
	}
}
