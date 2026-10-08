package tui

import (
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/api"
)

func TestFormatCostUSD(t *testing.T) {
	// Sub-cent costs keep four decimals so an early session is not stuck at
	// "$0.00"; from a cent up they round to cents.
	if got := formatCostUSD(0.0034); got != "$0.0034" {
		t.Errorf("formatCostUSD(0.0034) = %q, want $0.0034", got)
	}
	if got := formatCostUSD(1.4249); got != "$1.42" {
		t.Errorf("formatCostUSD(1.4249) = %q, want $1.42", got)
	}
	if got := formatCostUSD(0); got != "$0.0000" {
		t.Errorf("formatCostUSD(0) = %q, want $0.0000", got)
	}
}

func TestFormatCostStats(t *testing.T) {
	// Known model: the total plus the per-dimension breakdown that produced it.
	got := formatCostStats("claude-opus-5", api.Usage{InputTokens: 1000, OutputTokens: 2000}, 0)
	if !strings.Contains(got, "Cost: $") {
		t.Errorf("expected a dollar cost: %q", got)
	}
	if !strings.Contains(got, "input=1000") || !strings.Contains(got, "output=2000") {
		t.Errorf("expected token breakdown: %q", got)
	}
	// Unknown model: say so rather than print a misleading $0.00.
	unknown := formatCostStats("openai/gpt-oss-120b", api.Usage{OutputTokens: 5000}, 0)
	if !strings.Contains(unknown, "unknown") {
		t.Errorf("expected unknown-model note: %q", unknown)
	}
}
