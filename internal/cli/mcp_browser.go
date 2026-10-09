package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/greenthread-ai/klaudia/internal/browser"
	"github.com/greenthread-ai/klaudia/internal/browsermcp"
)

func newMcpCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "mcp",
		Short:         "MCP servers for an embedding host (Codex)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newMcpBrowserCommand())
	return cmd
}

func newMcpBrowserCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "browser",
		Short: "stdio MCP server exposing the shared headless Chrome (navigate, snapshot, fetch, search)",
		Long: `Serve Klaudia's browser engine over stdio MCP until stdin closes.

Codex config:

  [mcp_servers.browser]
  command = "klaudia"
  args = ["mcp", "browser"]
  tool_timeout_sec = 60

tool_timeout_sec needs to cover a headless Chrome launch plus a page load.
Whether Chrome or Chromium is installed in a given image is not checked here.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runMcpBrowser,
	}
}

func runMcpBrowser(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	engine := browser.DefaultEngine(ctx)
	defer engine.Close()
	return browsermcp.Serve(ctx, browsermcp.NewServer(engine), cmd.InOrStdin(), cmd.OutOrStdout())
}
