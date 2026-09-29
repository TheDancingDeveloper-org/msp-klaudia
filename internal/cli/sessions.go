package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/greenthread-ai/klaudia/internal/config"
	"github.com/greenthread-ai/klaudia/internal/session"
)

// Default retention when the config leaves it unset. Conservative: a user with
// a normal amount of history never has a session pruned by surprise, and the
// two caps only bite an accumulation that has clearly gone stale.
const (
	defaultRetentionDays = 30
	defaultRetentionMax  = 100
)

// newSessionsCommand builds `klaudia sessions` and its subcommands. Kept in its
// own file so the storage-management surface stays out of the main run wiring
// in root.go.
func newSessionsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List and remove saved sessions",
		Long: "Manage the session transcripts Klaudia stores under ~/.klaudia/sessions.\n" +
			"Old sessions are also pruned automatically at startup (see [sessions] in config).",
	}
	cmd.AddCommand(newSessionsLsCommand(), newSessionsRmCommand())
	return cmd
}

func newSessionsLsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List saved sessions (id, age, project, title)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			infos, err := session.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				// Never emit "null" for an empty store — an empty list is the
				// honest, parseable answer.
				if infos == nil {
					infos = []session.SessionInfo{}
				}
				return enc.Encode(infos)
			}
			if len(infos) == 0 {
				fmt.Fprintln(out, "No saved sessions.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAGE\tPROJECT\tTITLE")
			now := time.Now()
			for _, s := range infos {
				title := s.Title
				if title == "" {
					title = "(untitled)"
				}
				project := s.Project
				if project == "" {
					project = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, humanAge(now.Sub(s.Modified)), project, title)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the listing as JSON")
	return cmd
}

func newSessionsRmCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Delete a saved session (transcript, summary, and metadata)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !session.ValidID(id) {
				return usageErrorf("sessions rm %q: not a valid session id", id)
			}
			deleted, err := session.DeleteByID(id)
			if err != nil {
				return err
			}
			if !deleted {
				// Not a usage mistake — the command was well-formed — but still a
				// non-zero exit so a script can tell "gone" from "was never there".
				return fmt.Errorf("no session %q found", id)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted session %s\n", id)
			return nil
		},
	}
}

// humanAge renders a duration as a compact age like "3d", "5h", "12m", "just
// now" — enough to scan a list by, without a full timestamp column.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// sessionRetention resolves the config's retention settings into a policy,
// applying the conservative defaults for unset fields and honouring a negative
// value as "no cap for this dimension".
func sessionRetention(c config.Sessions) session.Retention {
	var r session.Retention
	switch {
	case c.RetentionDays < 0:
		r.MaxAge = 0
	case c.RetentionDays == 0:
		r.MaxAge = defaultRetentionDays * 24 * time.Hour
	default:
		r.MaxAge = time.Duration(c.RetentionDays) * 24 * time.Hour
	}
	switch {
	case c.RetentionMax < 0:
		r.MaxCount = 0
	case c.RetentionMax == 0:
		r.MaxCount = defaultRetentionMax
	default:
		r.MaxCount = c.RetentionMax
	}
	return r
}
