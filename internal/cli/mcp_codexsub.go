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

// newMcpCommand is `klaudia mcp`, the stdio MCP servers the klaudia binary
// hosts for other agents. Codex reaches Klaudia's worktree machinery through
// `klaudia mcp codex-subagent` rather than through a patch to Codex itself.
//
// The name matches the parent #299 introduces for `klaudia mcp browser`.
// Whichever of the two merges second drops its own copy and adds its
// subcommand to the one that landed.
func newMcpCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "mcp",
		Short:        "MCP servers hosted by the klaudia binary",
		SilenceUsage: true,
	}
	cmd.AddCommand(newMCPCodexSubagentCommand())
	return cmd
}

func newMCPCodexSubagentCommand() *cobra.Command {
	var bin, sandbox string
	var roots []string
	var maxDepth, maxJobs int
	cmd := &cobra.Command{
		Use:   "codex-subagent",
		Short: "MCP server: spawn a Codex child in a seeded worktree and adopt its changes",
		Long: `Serve spawn_isolated over stdio MCP until stdin closes.

The child always runs with an explicit posture, -s workspace-write -a never,
so it can write inside the checkout it is given and nowhere else, and a
command outside that sandbox is refused rather than escalated. --sandbox
widens it. That is a flag on this command, not a tool argument: the model
that calls the tool must not be able to widen its own child.

--codex defaults to whatever "codex" resolves to on PATH. On a Vogt pod that
is the full-access wrapper, which forces
--dangerously-bypass-approvals-and-sandbox and silently undoes the posture
above, so point --codex at the real binary there.

working_dir is confined to the directories named with --root, or to this
process's own working directory when none is named. A child cannot be pointed
at $HOME or at another repository.

Recursion is bounded by KLAUDIA_CODEXSUB_DEPTH, which this server sets in the
child's environment and reads from its own. A child that calls spawn_isolated
runs another copy of this server, and that copy refuses once the depth passes
--max-depth.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveCodexSubagent(cmd.Context(), serverOpts{
				Bin: bin, Sandbox: sandbox, Roots: roots, MaxDepth: maxDepth, MaxJobs: maxJobs,
			})
		},
	}
	cmd.Flags().StringVar(&bin, "codex", "", "codex executable to spawn (default: codex on PATH; not the full-access wrapper)")
	cmd.Flags().StringVar(&sandbox, "sandbox", "workspace-write", "sandbox posture passed to the child as -s (operator only)")
	cmd.Flags().StringArrayVar(&roots, "root", nil, "directory working_dir must fall inside (repeatable; default: the server's working directory)")
	cmd.Flags().IntVar(&maxDepth, "max-depth", 0, "how many times a child may itself spawn (0 refuses any nesting)")
	cmd.Flags().IntVar(&maxJobs, "max-jobs", 4, "maximum concurrent background jobs")
	return cmd
}

// serverOpts is the operator's configuration. None of it is reachable from a
// tool argument.
type serverOpts struct {
	Bin      string
	Sandbox  string
	Roots    []string
	MaxDepth int
	MaxJobs  int
}

// serveCodexSubagent runs the server on the process's own stdin and stdout,
// which is the only pair the SDK's stdio transport speaks on.
func serveCodexSubagent(ctx context.Context, opts serverOpts) error {
	depth := 0
	if v := os.Getenv(codexsub.DepthEnv); v != "" {
		fmt.Sscan(v, &depth)
	}
	// The runner's context dies when the server does, and every child is bound
	// to it, so a child still running when Codex closes the server is killed
	// rather than orphaned.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runner := &codexsub.Runner{
		Bin: opts.Bin, Sandbox: opts.Sandbox, Roots: opts.Roots,
		MaxDepth: opts.MaxDepth, MaxJobs: opts.MaxJobs, Depth: depth,
	}
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
