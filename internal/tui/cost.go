package tui

import (
	"fmt"

	"github.com/greenthread-ai/klaudia/internal/api"
)

// Cost formatting for the TUI. The dollar arithmetic and the price table live
// in internal/api (CostUSD); this file only formats, so tui.go stays small.

// sessionUsage gathers the cumulative session token counts the model tracks
// into the shape api.CostUSD prices.
func (m *Model) sessionUsage() api.Usage {
	return api.Usage{
		InputTokens:              m.statIn,
		OutputTokens:             m.statOut,
		CacheReadInputTokens:     m.statCacheRead,
		CacheCreationInputTokens: m.statCacheWrite,
	}
}

// sessionModel is the model id used to price the session. Empty when no session
// is attached, which CostUSD treats as the default model.
func (m *Model) sessionModel() string {
	if m.sess == nil {
		return ""
	}
	return m.sess.displayModel()
}

// formatCostUSD renders a cost compactly for the status bar. Sub-cent costs get
// four decimals ("$0.0034") so an early session is not stuck at "$0.00"; from a
// cent up it rounds to cents ("$1.42").
func formatCostUSD(usd float64) string {
	if usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// costSegment is the status-bar cost chunk, or "" when the model has no known
// price (nothing honest to show) — the segment simply drops out, like the ctx
// indicator does when the context window is unknown.
func (m *Model) costSegment() string {
	usd, known := api.CostUSD(m.sessionModel(), m.sessionUsage())
	if !known {
		return ""
	}
	return formatCostUSD(usd)
}

// formatCostStats renders the /stats cost line: the total plus the per-dimension
// token breakdown that produced it. Returns "" for an unpriced model so /stats
// shows only the token line rather than a misleading "$0.00". Standalone (no
// Model) so a test can pin the format.
func formatCostStats(model string, u api.Usage) string {
	usd, known := api.CostUSD(model, u)
	if !known {
		return fmt.Sprintf("Cost: unknown (no price for model %q)", model)
	}
	return fmt.Sprintf("Cost: %s  (input=%d  output=%d  cache_read=%d  cache_write=%d)",
		formatCostUSD(usd), u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
}
