package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/greenthread-ai/klaudia/internal/api"
)

// Choosing a model by typing its exact id means knowing the id, and model ids
// change more often than anyone's memory of them does. Both backends Klaudia
// speaks to can enumerate what they serve, so /model asks the endpoint and
// offers the answer as a picker; typing an id still works for pinning something
// the endpoint doesn't list.

// modelFetchTimeout bounds the lookup so a wedged endpoint can't leave /model
// hanging with no way back.
const modelFetchTimeout = 10 * time.Second

// fetchModels asks the provider for its model list off the UI goroutine.
func (m *Model) fetchModels() tea.Cmd {
	list := m.sess.ListModels
	parent := m.ctx
	if parent == nil {
		parent = context.Background()
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, modelFetchTimeout)
		defer cancel()
		models, err := list(ctx)
		return modelsMsg{models: models, err: err}
	}
}

// showModelPicker turns the fetched list into the standard picker.
func (m *Model) showModelPicker(models []api.ModelInfo) {
	if len(models) == 0 {
		m.appendLine(bannerStyle.Render("The provider reported no models. /model <id> still works."))
		return
	}
	current := api.ResolveModelFor(m.sess.Provider, m.sess.Model)
	items := make([]choiceItem, 0, len(models))
	for _, mi := range models {
		mi := mi
		label := mi.Name()
		if mi.Name() != mi.ID {
			label += "  " + mi.ID
		}
		if mi.ContextWindow > 0 {
			label += fmt.Sprintf("  · %s ctx", humanTokens(int64(mi.ContextWindow)))
		}
		if mi.ID == string(current) {
			label += "  (current)"
		}
		items = append(items, choiceItem{
			label: label,
			apply: func() string { return m.setModel(mi.ID, mi.ContextWindow) },
		})
	}

	// Every model is offered: the picker scrolls and filters (picker.go), so an
	// endpoint serving dozens of models no longer loses all but nine of them.
	m.startChoice("Select a model:", items)
}

// setModel switches the model for subsequent turns. A context window from the
// provider is recorded alongside it: that figure is authoritative, unlike the
// static per-model table, so the status bar's `ctx N%` tracks the model the user
// actually chose.
func (m *Model) setModel(id string, contextWindow int) string {
	id = strings.TrimSpace(id)
	m.sess.Model = id
	// Aliases resolve only on Anthropic; another provider is sent the id as
	// typed, so a bare "sonnet" there gets a warning instead of a rewrite.
	m.sess.ResolvedModel = string(api.ResolveModelFor(m.sess.Provider, id))

	msg := "Model set to " + id
	if resolved := m.sess.ResolvedModel; resolved != id {
		msg += " (" + resolved + ")"
	}
	if contextWindow > 0 {
		m.sess.ContextWindow = contextWindow
		m.sess.ContextWindowSource = "provider"
	} else if limit, source := api.ContextWindowFor(m.sess.Provider, id, 0); limit > 0 {
		m.sess.ContextWindow, m.sess.ContextWindowSource = limit, source
	}
	if m.sess.ContextWindow > 0 {
		msg += fmt.Sprintf(" · %s context", humanTokens(int64(m.sess.ContextWindow)))
	}
	msg += ". Applies to the next turn."
	if w := api.AliasWarning(m.sess.Provider, id); w != "" {
		msg += "\nwarning: " + w + "."
	}
	return msg
}

// unlistedModelWarning checks a typed /model id against the list the provider
// reported the last time /model fetched it, and returns a warning when the id
// is not in it. It never fetches: a /model <id> that waited on the network
// would be slower than finding out on the next turn, and when nothing has been
// fetched yet there is nothing to say. It warns rather than refuses, because
// an endpoint can serve ids it does not list (aliases, dated snapshots).
func (m *Model) unlistedModelWarning(id string) string {
	if len(m.knownModels) == 0 {
		return ""
	}
	id = strings.TrimSpace(id)
	resolved := string(api.ResolveModel(id))
	for _, mi := range m.knownModels {
		if mi.ID == id || mi.ID == resolved {
			return ""
		}
	}
	return "  " + id + " is not in the list this endpoint reported; if the next turn says the model was not found, run /model to pick one it lists."
}
