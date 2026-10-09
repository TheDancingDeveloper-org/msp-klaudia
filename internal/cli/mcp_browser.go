package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/greenthread-ai/klaudia/internal/browser"
	"github.com/greenthread-ai/klaudia/internal/browsermcp"
)

func newMcpBrowserCommand() *cobra.Command {
	var allowPrivate bool
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "stdio MCP server exposing the shared headless Chrome (navigate, snapshot, fetch, search)",
		Long: `Serve Klaudia's browser engine over stdio MCP until stdin closes.

Codex config:

  [mcp_servers.browser]
  command = "klaudia"
  args = ["mcp", "browser"]
  tool_timeout_sec = 60

tool_timeout_sec needs to cover a headless Chrome launch plus a page load.
Whether Chrome or Chromium is installed in a given image is not checked here.

Navigation to loopback, link-local, RFC1918, carrier-grade NAT and
unique-local addresses is refused, including after a redirect, because Codex
runs this server outside its sandbox. --allow-private lifts that. It is a
flag on this command, not a tool argument.

Search stays headless: the headed-Chrome fallback the in-process tools use is
off here, since this server has no display to fall back to.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMcpBrowser(cmd, allowPrivate)
		},
	}
	cmd.Flags().BoolVar(&allowPrivate, "allow-private", false, "permit navigation to private and local addresses (loopback, link-local, RFC1918, 100.64/10, fc00::/7)")
	return cmd
}

func runMcpBrowser(cmd *cobra.Command, allowPrivate bool) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	engine := browser.DefaultEngine(ctx)
	opts := engine.Options()
	// A headed window cannot be opened from an MCP server Codex launched
	// headless, and must not be: the fallback would put a browser on a
	// display the operator is not watching.
	opts.HeadedFallback = false
	opts.Headless = true
	engine = browser.NewEngine(ctx, opts)
	defer engine.Close()
	return browsermcp.Serve(ctx, browsermcp.NewServer(engine, browsermcp.ServerOpts{AllowPrivate: allowPrivate}), cmd.InOrStdin(), cmd.OutOrStdout())
}
