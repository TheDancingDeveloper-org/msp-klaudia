package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/greenthread-ai/klaudia/internal/agent"
)

// The /agents view has two halves: the sub-agent types the model can launch
// (unchanged), and the background sub-agents actually running or finished. The
// second half is why /agents is more than a static list now — a background
// launch is invisible otherwise, exactly the closed-box problem progress lines
// fixed for the synchronous case.

// BackgroundAgentLister exposes the background sub-agents to the view without
// the TUI owning the registry. *agent.BackgroundRegistry satisfies it.
type BackgroundAgentLister interface {
	List() []agent.BackgroundAgent
	// Get returns one agent by id. *agent.BackgroundRegistry satisfies it.
	Get(id string) (agent.BackgroundAgent, bool)
	// Cancel stops a running agent. Returns false when the id is unknown or
	// already finished.
	Cancel(id string) bool
}

// renderBackgroundAgents formats the running/finished background sub-agents:
// id, type, status, label, and elapsed time. Returns "" when there are none, so
// /agents stays quiet about a feature the session never used.
func renderBackgroundAgents(agents []agent.BackgroundAgent) string {
	if len(agents) == 0 {
		return ""
	}
	// Running first, then the rest in launch order — what is still working is
	// what the user is most likely asking about.
	sorted := append([]agent.BackgroundAgent(nil), agents...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := !sorted[i].Done(), !sorted[j].Done()
		if ri != rj {
			return ri
		}
		return sorted[i].ID < sorted[j].ID
	})

	var b strings.Builder
	b.WriteString("Sub-agents:")
	for _, a := range sorted {
		fmt.Fprintf(&b, "\n  %-9s %-16s %-9s %s",
			a.ID, a.Type, a.Status, fmtAgentElapsed(a.Elapsed()))
		if a.Label != "" {
			fmt.Fprintf(&b, "  %s", a.Label)
		}
		if a.Provenance != "" {
			fmt.Fprintf(&b, "\n              cut from %s", oneLine(a.Provenance, 72))
		}
		switch {
		case a.Status == agent.BackgroundFailed && a.Err != "":
			fmt.Fprintf(&b, "\n              %s", oneLine(a.Err, 72))
		case !a.Done() && a.Activity != "":
			fmt.Fprintf(&b, "\n              … %s", oneLine(a.Activity, 72))
		}
	}
	return b.String()
}

// fmtAgentElapsed renders a duration compactly (e.g. "12s", "3m04s").
func fmtAgentElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}

// oneLine flattens whitespace and clips to n runes, so an error or activity
// string cannot break the one-line-per-agent shape.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// backgroundAgentsSection returns the background-agents block for /agents, or ""
// when the session has no lister or no agents.
func (m *Model) backgroundAgentsSection() string {
	if m.sess == nil || m.sess.BackgroundAgents == nil {
		return ""
	}
	return renderBackgroundAgents(m.sess.BackgroundAgents.List())
}

// agentsCommand handles /agents, /agents show <id> and /agents cancel <id>.
// Bare /agents lists the types and every child, foreground and background.
func (m *Model) agentsCommand(args []string) string {
	if len(args) == 0 {
		out := m.renderAgents()
		if bg := m.backgroundAgentsSection(); bg != "" {
			out += "\n\n" + bg
		}
		return out
	}
	if m.sess == nil || m.sess.BackgroundAgents == nil {
		return "No sub-agents are tracked in this session."
	}
	switch args[0] {
	case "show":
		if len(args) < 2 {
			return "/agents show <id>"
		}
		a, ok := m.sess.BackgroundAgents.Get(args[1])
		if !ok {
			return "No sub-agent " + args[1]
		}
		return renderAgentDetail(a)
	case "cancel":
		if len(args) < 2 {
			return "/agents cancel <id>"
		}
		if !m.sess.BackgroundAgents.Cancel(args[1]) {
			return "Cannot cancel " + args[1] + " (unknown, or already finished)"
		}
		return "Cancelling " + args[1]
	default:
		return "Unknown /agents command " + args[0] + ". Try /agents show <id> or /agents cancel <id>."
	}
}

// renderAgentDetail is the /agents show view: the status line plus the result
// or the error, which the list truncates.
func renderAgentDetail(a agent.BackgroundAgent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s  %s", a.ID, a.Type, a.Status, fmtAgentElapsed(a.Elapsed()))
	if a.Label != "" {
		fmt.Fprintf(&b, "  %s", a.Label)
	}
	if !a.Background {
		b.WriteString("\nforeground")
	}
	if a.Provenance != "" {
		fmt.Fprintf(&b, "\ncut from %s", a.Provenance)
	}
	switch a.Status {
	case agent.BackgroundFailed:
		if a.Err != "" {
			fmt.Fprintf(&b, "\n\n%s", a.Err)
		}
		if a.Result != "" {
			fmt.Fprintf(&b, "\n\n%s", a.Result)
		}
	default:
		if a.Result != "" {
			fmt.Fprintf(&b, "\n\n%s", a.Result)
		} else if a.Activity != "" {
			fmt.Fprintf(&b, "\n\n… %s", a.Activity)
		}
	}
	return b.String()
}
