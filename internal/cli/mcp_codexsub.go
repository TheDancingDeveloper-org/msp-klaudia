package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/greenthread-ai/klaudia/internal/codexsub"
	"github.com/greenthread-ai/klaudia/internal/version"
)

// NewMCPCommand is `klaudia mcp`, the stdio MCP servers the klaudia binary
// hosts for other agents. Codex reaches Klaudia's worktree machinery through
// `klaudia mcp codex-subagent` rather than through a patch to Codex itself.
func NewMCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "mcp",
		Short:        "MCP servers hosted by the klaudia binary",
		SilenceUsage: true,
	}
	cmd.AddCommand(newMCPCodexSubagentCommand())
	return cmd
}

func newMCPCodexSubagentCommand() *cobra.Command {
	var bin string
	cmd := &cobra.Command{
		Use:   "codex-subagent",
		Short: "MCP server: spawn a Codex child in a seeded worktree and adopt its changes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveCodexSubagent(cmd.Context(), bin)
		},
	}
	cmd.Flags().StringVar(&bin, "codex", "", "codex executable to spawn (default: codex on PATH)")
	return cmd
}

// serveCodexSubagent runs the server on the process's own stdin and stdout,
// which is the only pair the SDK's stdio transport speaks on.
func serveCodexSubagent(ctx context.Context, bin string) error {
	runner := &codexsub.Runner{Bin: bin}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "klaudia-codex-subagent",
		Version: version.Version,
	}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "spawn_isolated",
		Description: "Run a Codex child on a task. isolation=worktree (the default) " +
			"seeds a git worktree from working_dir's uncommitted state and adopts " +
			"the child's changes back, reporting conflicts. isolation=shared runs " +
			"in working_dir directly. background=true returns a job id; call again " +
			"with job set to poll, or with wait=true to block until it finishes. " +
			"output_schema, when given, is a JSON Schema the child's final message " +
			"must satisfy; a mismatch adopts nothing.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, req codexsub.Request) (*mcpsdk.CallToolResult, any, error) {
		res, err := runner.Run(ctx, req)
		if err != nil {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
				IsError: true,
			}, nil, nil
		}
		body, merr := json.Marshal(res)
		if merr != nil {
			return nil, nil, merr
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(body)}},
		}, res, nil
	})

	// Logging goes to stderr. Stdout is the MCP transport, and a log line there
	// is a protocol error the client reports as a broken server.
	log.SetOutput(os.Stderr)
	if err := server.Run(ctx, &mcpsdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}
