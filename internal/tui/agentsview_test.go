package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/greenthread-ai/klaudia/internal/agent"
)

type fakeBackgroundAgents struct{ agents []agent.BackgroundAgent }

func (f *fakeBackgroundAgents) List() []agent.BackgroundAgent { return f.agents }
func (f *fakeBackgroundAgents) Get(string) (agent.BackgroundAgent, bool) {
	return agent.BackgroundAgent{}, false
}
func (f *fakeBackgroundAgents) Cancel(string) bool { return false }

// The /agents view lists each background agent with its id, type, status and
// elapsed time — the status view the issue asks for.
func TestRenderBackgroundAgentsListsStates(t *testing.T) {
	now := time.Now()
	out := renderBackgroundAgents([]agent.BackgroundAgent{
		{
			ID: "agent-1", Type: "Explore", Label: "search callers",
			Status: agent.BackgroundSucceeded, StartedAt: now.Add(-90 * time.Second),
			FinishedAt: now.Add(-30 * time.Second), Result: "done",
		},
		{
			ID: "agent-2", Type: "general-purpose", Label: "refactor api",
			Status: agent.BackgroundRunning, StartedAt: now.Add(-10 * time.Second),
			Activity: "Edit internal/api/client.go", Isolated: true,
		},
	})
	for _, want := range []string{"agent-1", "Explore", "succeeded", "search callers",
		"agent-2", "general-purpose", "running", "1m00s"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q:\n%s", want, out)
		}
	}
	// A running agent shows its current activity.
	if !strings.Contains(out, "Edit internal/api/client.go") {
		t.Errorf("running agent's activity not shown:\n%s", out)
	}
}

// The view names the repository, branch and HEAD a child was cut from, so a
// background launch into another checkout is visible without opening it.
func TestRenderBackgroundAgentsShowsProvenance(t *testing.T) {
	out := renderBackgroundAgents([]agent.BackgroundAgent{{
		ID: "agent-1", Type: "general-purpose", Status: agent.BackgroundRunning,
		Provenance: "/repos/other on feature at abc1234",
	}})
	if !strings.Contains(out, "cut from /repos/other on feature at abc1234") {
		t.Errorf("provenance not shown:\n%s", out)
	}
}

// Running agents are listed before finished ones.
func TestRenderBackgroundAgentsRunningFirst(t *testing.T) {
	out := renderBackgroundAgents([]agent.BackgroundAgent{
		{ID: "agent-1", Type: "Explore", Status: agent.BackgroundSucceeded},
		{ID: "agent-2", Type: "Explore", Status: agent.BackgroundRunning},
	})
	if strings.Index(out, "agent-2") > strings.Index(out, "agent-1") {
		t.Errorf("running agent should sort before the finished one:\n%s", out)
	}
}

// No background agents means the view says nothing (only the type list shows).
func TestRenderBackgroundAgentsEmpty(t *testing.T) {
	if got := renderBackgroundAgents(nil); got != "" {
		t.Errorf("empty listing should render nothing, got %q", got)
	}
}

// A failed agent's error is surfaced in the view.
func TestRenderBackgroundAgentsShowsError(t *testing.T) {
	out := renderBackgroundAgents([]agent.BackgroundAgent{
		{ID: "agent-1", Type: "general-purpose", Status: agent.BackgroundFailed, Err: "could not isolate a worktree"},
	})
	if !strings.Contains(out, "could not isolate a worktree") {
		t.Errorf("failed agent's error not shown:\n%s", out)
	}
}

// The /agents handler appends the background section to the type list when the
// session has a lister.
func TestAgentsCommandIncludesBackgroundSection(t *testing.T) {
	m := &Model{sess: &Session{
		Agents: []AgentInfo{{Name: "Explore", Description: "read only"}},
		BackgroundAgents: &fakeBackgroundAgents{agents: []agent.BackgroundAgent{
			{ID: "agent-1", Type: "Explore", Status: agent.BackgroundRunning, StartedAt: time.Now()},
		}},
	}, height: 40}
	got := m.renderAgents()
	if sec := m.backgroundAgentsSection(); sec != "" {
		got += "\n\n" + sec
	}
	if !strings.Contains(got, "Explore") || !strings.Contains(got, "agent-1") {
		t.Errorf("combined /agents output missing a half:\n%s", got)
	}
}

// fakeLister is the registry the /agents commands talk to.
type fakeLister struct {
	agents  []agent.BackgroundAgent
	cancels []string
}

func (f *fakeLister) List() []agent.BackgroundAgent { return f.agents }
func (f *fakeLister) Get(id string) (agent.BackgroundAgent, bool) {
	for _, a := range f.agents {
		if a.ID == id {
			return a, true
		}
	}
	return agent.BackgroundAgent{}, false
}
func (f *fakeLister) Cancel(id string) bool {
	f.cancels = append(f.cancels, id)
	return id == "agent-1"
}

func agentsModel(l BackgroundAgentLister) *Model {
	return &Model{sess: &Session{BackgroundAgents: l}}
}

// /agents show prints the child's result in full, which the list truncates.
func TestAgentsShowPrintsResult(t *testing.T) {
	m := agentsModel(&fakeLister{agents: []agent.BackgroundAgent{{
		ID: "agent-1", Type: "Explore", Status: agent.BackgroundSucceeded,
		Result: "the full report",
	}}})
	out := m.agentsCommand([]string{"show", "agent-1"})
	if !strings.Contains(out, "the full report") {
		t.Errorf("show did not print the result:\n%s", out)
	}
	if got := m.agentsCommand([]string{"show", "agent-9"}); !strings.Contains(got, "No sub-agent") {
		t.Errorf("unknown id: %q", got)
	}
}

// /agents cancel stops a running child and says so; an unknown id is refused.
func TestAgentsCancel(t *testing.T) {
	l := &fakeLister{}
	m := agentsModel(l)
	if got := m.agentsCommand([]string{"cancel", "agent-1"}); !strings.Contains(got, "Cancelling") {
		t.Errorf("cancel: %q", got)
	}
	if len(l.cancels) != 1 || l.cancels[0] != "agent-1" {
		t.Errorf("cancels = %v", l.cancels)
	}
	if got := m.agentsCommand([]string{"cancel", "agent-9"}); !strings.Contains(got, "Cannot cancel") {
		t.Errorf("unknown cancel: %q", got)
	}
}
