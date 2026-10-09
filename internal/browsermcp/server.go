// Package browsermcp exposes internal/browser as a stdio MCP server so a
// host that is not Klaudia — Codex — can drive the same headless Chrome.
package browsermcp

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/greenthread-ai/klaudia/internal/browser"
	"github.com/greenthread-ai/klaudia/internal/version"
)

// Engine is the slice of browser.Engine the tools use. *browser.Engine
// satisfies it; tests substitute a fake so the suite never launches Chrome.
type Engine interface {
	Navigate(url string) error
	Snapshot(includeHTML, includeMarkdown bool) (*browser.Snapshot, error)
	Search(ctx context.Context, opts browser.SearchOptions) ([]browser.SearchResult, error)
	Close()
}

// NewServer registers the four browser tools on a fresh MCP server. One
// engine is shared by every call for the life of the process.
func NewServer(engine Engine) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "klaudia-browser",
		Version: version.Version,
	}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_navigate",
		Description: "Navigate the shared headless Chrome to an absolute http or https URL and return the resulting page URL and title.",
	}, navigate(engine))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_snapshot",
		Description: "Capture the shared browser's current page. Set include_html and/or include_markdown for the rendered content.",
	}, snapshot(engine))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_fetch",
		Description: "Navigate to an absolute http or https URL and return the page as markdown.",
	}, fetch(engine))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "web_search",
		Description: "Search the web and return titles, URLs and snippets. Uses the engine's configured search backend.",
	}, search(engine))
	return s
}

// Serve runs the server on r and w until the client disconnects (stdin
// closing, for the stdio command). The caller closes the engine.
func Serve(ctx context.Context, s *mcp.Server, r io.Reader, w io.Writer) error {
	tr := &mcp.IOTransport{Reader: nopCloser{r}, Writer: nopWriteCloser{w}}
	return s.Run(ctx, tr)
}

// nopCloser lets a plain reader satisfy IOTransport without the server
// closing the process's stdin when the session ends.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type navigateIn struct {
	URL string `json:"url" jsonschema:"the absolute http or https URL to open"`
}

type pageOut struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	HTML     string `json:"html,omitempty"`
	Markdown string `json:"markdown,omitempty"`
}

func navigate(e Engine) mcp.ToolHandlerFor[navigateIn, pageOut] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in navigateIn) (*mcp.CallToolResult, pageOut, error) {
		if err := validateHTTPURL(in.URL); err != nil {
			return nil, pageOut{}, err
		}
		if err := e.Navigate(in.URL); err != nil {
			return nil, pageOut{}, err
		}
		snap, err := e.Snapshot(false, false)
		if err != nil {
			return nil, pageOut{}, err
		}
		return nil, pageFrom(snap), nil
	}
}

type snapshotIn struct {
	IncludeHTML     bool `json:"include_html,omitempty" jsonschema:"include the rendered page HTML"`
	IncludeMarkdown bool `json:"include_markdown,omitempty" jsonschema:"include the page as markdown"`
}

func snapshot(e Engine) mcp.ToolHandlerFor[snapshotIn, pageOut] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in snapshotIn) (*mcp.CallToolResult, pageOut, error) {
		snap, err := e.Snapshot(in.IncludeHTML, in.IncludeMarkdown)
		if err != nil {
			return nil, pageOut{}, err
		}
		return nil, pageFrom(snap), nil
	}
}

type fetchIn struct {
	URL string `json:"url" jsonschema:"the absolute http or https URL to fetch as markdown"`
}

type fetchOut struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

func fetch(e Engine) mcp.ToolHandlerFor[fetchIn, fetchOut] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in fetchIn) (*mcp.CallToolResult, fetchOut, error) {
		if err := validateHTTPURL(in.URL); err != nil {
			return nil, fetchOut{}, err
		}
		if err := e.Navigate(in.URL); err != nil {
			return nil, fetchOut{}, err
		}
		snap, err := e.Snapshot(false, true)
		if err != nil {
			return nil, fetchOut{}, err
		}
		return nil, fetchOut{URL: snap.URL, Title: snap.Title, Markdown: snap.Markdown}, nil
	}
}

type searchIn struct {
	Query          string   `json:"query" jsonschema:"the search query"`
	Engine         string   `json:"engine,omitempty" jsonschema:"search backend, for example ddg or google; empty uses the configured default"`
	AllowedDomains []string `json:"allowed_domains,omitempty" jsonschema:"only return results on these domains"`
	BlockedDomains []string `json:"blocked_domains,omitempty" jsonschema:"drop results on these domains"`
	MaxResults     int      `json:"max_results,omitempty" jsonschema:"maximum results to return (1 to 20)"`
}

type searchOut struct {
	Results []browser.SearchResult `json:"results"`
}

func search(e Engine) mcp.ToolHandlerFor[searchIn, searchOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		if strings.TrimSpace(in.Query) == "" {
			return nil, searchOut{}, fmt.Errorf("query required")
		}
		results, err := e.Search(ctx, browser.SearchOptions{
			Engine:         in.Engine,
			Query:          in.Query,
			AllowedDomains: in.AllowedDomains,
			BlockedDomains: in.BlockedDomains,
			MaxResults:     in.MaxResults,
		})
		if err != nil {
			return nil, searchOut{}, err
		}
		if results == nil {
			results = []browser.SearchResult{}
		}
		return nil, searchOut{Results: results}, nil
	}
}

func pageFrom(s *browser.Snapshot) pageOut {
	if s == nil {
		return pageOut{}
	}
	return pageOut{URL: s.URL, Title: s.Title, HTML: s.HTML, Markdown: s.Markdown}
}

// validateHTTPURL is the same check internal/tools uses for BrowserNavigate.
// It stays unexported there; copying these few lines keeps this package from
// depending on the tool registry.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("url must be an absolute http or https URL")
	}
	return nil
}
