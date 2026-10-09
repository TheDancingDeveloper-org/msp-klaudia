// Package browsermcp exposes internal/browser as a stdio MCP server so a
// host that is not Klaudia — Codex — can drive the same headless Chrome.
package browsermcp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/greenthread-ai/klaudia/internal/browser"
	"github.com/greenthread-ai/klaudia/internal/textsafe"
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

// pageBytes is how much rendered HTML or markdown one tool result may carry.
// Past that, the rest is dropped and the marker says so.
const pageBytes = 200_000

const truncated = "\n…[truncated]"

// ServerOpts is what the operator sets on the command line. Nothing here is a
// tool argument: the model cannot turn a restriction off by asking.
type ServerOpts struct {
	// AllowPrivate permits navigation to loopback, link-local, RFC1918,
	// carrier-grade NAT (100.64/10) and unique-local IPv6 (fc00::/7). Off by
	// default, because Codex runs this server outside its sandbox.
	AllowPrivate bool
}

// NewServer registers the four browser tools on a fresh MCP server. One
// engine is shared by every call for the life of the process, and every call
// that reads or changes the page takes the same lock, so a snapshot cannot
// land on a page a concurrent navigation just opened.
func NewServer(engine Engine, opts ServerOpts) *mcp.Server {
	h := &handlers{engine: engine, opts: opts}
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "klaudia-browser",
		Version: version.Version,
	}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_navigate",
		Description: "Navigate the shared headless Chrome to an absolute http or https URL and return the resulting page URL and title. Private and local addresses are refused unless the server was started with --allow-private.",
	}, h.navigate)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_snapshot",
		Description: "Capture the shared browser's current page. Set include_html and/or include_markdown for the rendered content.",
	}, h.snapshot)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_fetch",
		Description: "Navigate to an absolute http or https URL and return the page as markdown. Private and local addresses are refused unless the server was started with --allow-private.",
	}, h.fetch)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "web_search",
		Description: "Search the web and return titles, URLs and snippets. Uses the engine's configured search backend.",
	}, h.search)
	return s
}

// handlers shares one lock across the tools that touch the page. Search takes
// it too: it navigates the same engine.
type handlers struct {
	engine Engine
	opts   ServerOpts
	mu     sync.Mutex
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

func (h *handlers) navigate(_ context.Context, _ *mcp.CallToolRequest, in navigateIn) (*mcp.CallToolResult, pageOut, error) {
	if err := validateHTTPURL(in.URL); err != nil {
		return nil, pageOut{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.guard(in.URL); err != nil {
		return nil, pageOut{}, err
	}
	if err := h.engine.Navigate(in.URL); err != nil {
		return nil, pageOut{}, err
	}
	snap, err := h.engine.Snapshot(false, false)
	if err != nil {
		return nil, pageOut{}, err
	}
	if err := h.guard(snap.URL); err != nil {
		return nil, pageOut{}, fmt.Errorf("refusing the page Chrome landed on: %w", err)
	}
	return nil, pageFrom(snap), nil
}

type snapshotIn struct {
	IncludeHTML     bool `json:"include_html,omitempty" jsonschema:"include the rendered page HTML"`
	IncludeMarkdown bool `json:"include_markdown,omitempty" jsonschema:"include the page as markdown"`
}

func (h *handlers) snapshot(_ context.Context, _ *mcp.CallToolRequest, in snapshotIn) (*mcp.CallToolResult, pageOut, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap, err := h.engine.Snapshot(in.IncludeHTML, in.IncludeMarkdown)
	if err != nil {
		return nil, pageOut{}, err
	}
	if err := h.guard(snap.URL); err != nil {
		return nil, pageOut{}, fmt.Errorf("refusing the page Chrome is on: %w", err)
	}
	return nil, pageFrom(snap), nil
}

type fetchIn struct {
	URL string `json:"url" jsonschema:"the absolute http or https URL to fetch as markdown"`
}

type fetchOut struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

func (h *handlers) fetch(_ context.Context, _ *mcp.CallToolRequest, in fetchIn) (*mcp.CallToolResult, fetchOut, error) {
	if err := validateHTTPURL(in.URL); err != nil {
		return nil, fetchOut{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.guard(in.URL); err != nil {
		return nil, fetchOut{}, err
	}
	if err := h.engine.Navigate(in.URL); err != nil {
		return nil, fetchOut{}, err
	}
	snap, err := h.engine.Snapshot(false, true)
	if err != nil {
		return nil, fetchOut{}, err
	}
	if err := h.guard(snap.URL); err != nil {
		return nil, fetchOut{}, fmt.Errorf("refusing the page Chrome landed on: %w", err)
	}
	out := fetchOut{URL: snap.URL, Title: snap.Title, Markdown: snap.Markdown}
	out.Markdown = textsafe.Truncate(textsafe.StripInvisible(out.Markdown), pageBytes, truncated)
	return nil, out, nil
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

func (h *handlers) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, searchOut{}, fmt.Errorf("query required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	results, err := h.engine.Search(ctx, browser.SearchOptions{
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

func pageFrom(s *browser.Snapshot) pageOut {
	if s == nil {
		return pageOut{}
	}
	return pageOut{
		URL:      s.URL,
		Title:    s.Title,
		HTML:     textsafe.Truncate(textsafe.StripInvisible(s.HTML), pageBytes, truncated),
		Markdown: textsafe.Truncate(textsafe.StripInvisible(s.Markdown), pageBytes, truncated),
	}
}

// guard resolves the host and refuses an address that is not a public unicast
// destination, unless the server was started with --allow-private. A literal
// IP is checked directly. A name is resolved, and every address it returns
// must pass: one public address does not excuse a private one.
func (h *handlers) guard(raw string) error {
	if h.opts.AllowPrivate {
		return nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("refusing %q: not an absolute URL", raw)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return refusePrivate(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("refusing %q: cannot resolve %s: %w", raw, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("refusing %q: %s resolved to no addresses", raw, host)
	}
	for _, ip := range ips {
		if err := refusePrivate(ip); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
	}
	return nil
}

// blockedNets are the ranges a navigation must not reach unless the server
// was started with --allow-private. net.IP.IsPrivate covers RFC1918 and
// fc00::/7 only; the rest are named because Codex runs this server outside
// its sandbox, so a model-chosen URL is otherwise an open path onto the
// operator's network.
var blockedNets = mustCIDRs(
	"0.0.0.0/8",      // "this" network, includes the unspecified address
	"10.0.0.0/8",     // RFC1918
	"100.64.0.0/10",  // carrier-grade NAT
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local
	"172.16.0.0/12",  // RFC1918
	"192.168.0.0/16", // RFC1918
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved
	"::/128",         // unspecified
	"::1/128",        // loopback
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"ff00::/8",       // multicast
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, len(cidrs))
	for i, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out[i] = n
	}
	return out
}

func refusePrivate(ip net.IP) error {
	// An IPv4-mapped address (::ffff:10.1.2.3) would miss every IPv4 range
	// above, because those nets are four bytes and Contains refuses a
	// mismatch. Compare it as the IPv4 it names.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if !ip.IsGlobalUnicast() {
		return fmt.Errorf("refusing private or non-routable address %s", ip)
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return fmt.Errorf("refusing private or non-routable address %s", ip)
		}
	}
	return nil
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
