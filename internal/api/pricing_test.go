package api

import (
	"math"
	"testing"
)

// costEpsilon is the tolerance for float dollar comparisons — well below a
// hundredth of a cent, so a genuine rate error still trips the test.
const costEpsilon = 1e-9

func approxEqual(a, b float64) bool { return math.Abs(a-b) < costEpsilon }

func TestCostUSDKnownModels(t *testing.T) {
	tests := []struct {
		name  string
		model string
		usage Usage
		want  float64
	}{
		{
			name:  "opus-5 one million input tokens is the input rate",
			model: "claude-opus-5",
			usage: Usage{InputTokens: 1_000_000},
			want:  5.0,
		},
		{
			name:  "opus-5 one million output tokens is the output rate",
			model: "claude-opus-5",
			usage: Usage{OutputTokens: 1_000_000},
			want:  25.0,
		},
		{
			name:  "haiku one million input tokens is the input rate",
			model: "claude-haiku-4-5",
			usage: Usage{InputTokens: 1_000_000},
			want:  1.0,
		},
		{
			name:  "fable one million output tokens is the output rate",
			model: "claude-fable-5",
			usage: Usage{OutputTokens: 1_000_000},
			want:  50.0,
		},
		{
			name:  "sonnet-5 cache read and write priced at 0.1x and 1.25x input",
			model: "claude-sonnet-5",
			usage: Usage{CacheReadInputTokens: 1_000_000, CacheCreationInputTokens: 1_000_000},
			want:  0.20 + 2.50,
		},
		{
			name:  "opus-5 mixed usage sums every dimension",
			model: "claude-opus-5",
			usage: Usage{InputTokens: 1000, OutputTokens: 2000, CacheReadInputTokens: 500, CacheCreationInputTokens: 100},
			// 1000*5 + 2000*25 + 500*0.5 + 100*6.25, all /1e6.
			want: 0.005 + 0.05 + 0.00025 + 0.000625,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, known := CostUSD(tc.model, tc.usage)
			if !known {
				t.Fatalf("CostUSD(%q) reported unknown; want known", tc.model)
			}
			if !approxEqual(got, tc.want) {
				t.Errorf("CostUSD(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestCostUSDAliasesPriceLikeResolvedID(t *testing.T) {
	// A CLI alias must cost exactly what the ID it resolves to costs, so a user
	// is not billed differently for typing "opus" instead of the full snapshot.
	u := Usage{InputTokens: 12_345, OutputTokens: 6_789, CacheReadInputTokens: 400, CacheCreationInputTokens: 55}
	pairs := [][2]string{
		{"opus", "claude-opus-5"},
		{"sonnet", "claude-sonnet-5"},
		{"haiku", "claude-haiku-4-5"},
		{"fable", "claude-fable-5"},
		{"OPUS", "claude-opus-5"},
	}
	for _, p := range pairs {
		aliasCost, aliasKnown := CostUSD(p[0], u)
		fullCost, fullKnown := CostUSD(p[1], u)
		if aliasKnown != fullKnown || !approxEqual(aliasCost, fullCost) {
			t.Errorf("CostUSD(%q)=%v(known=%v) != CostUSD(%q)=%v(known=%v)",
				p[0], aliasCost, aliasKnown, p[1], fullCost, fullKnown)
		}
	}
}

func TestCostUSDUnknownModelIsZeroAndFlagged(t *testing.T) {
	// An OpenAI-compatible or otherwise unpriced model has no rate: cost must be
	// exactly 0 and known must be false, so callers can render "unknown" rather
	// than a misleading "$0.00" and a budget stop can never fire on it.
	for _, model := range []string{"openai/gpt-oss-120b", "some-future-model", "gpt-4"} {
		usd, known := CostUSD(model, Usage{InputTokens: 5_000_000, OutputTokens: 5_000_000})
		if known {
			t.Errorf("CostUSD(%q) reported known; want unknown", model)
		}
		if usd != 0 {
			t.Errorf("CostUSD(%q) = %v, want 0 for unknown model", model, usd)
		}
	}
}

func TestCostUSDAccumulates(t *testing.T) {
	// Cost is linear in tokens, so pricing the summed usage of several turns must
	// equal the sum of pricing each turn — this is what lets the loop keep a
	// single running total instead of accumulating per-turn dollar figures.
	const model = "claude-opus-5"
	turns := []Usage{
		{InputTokens: 1000, OutputTokens: 500, CacheReadInputTokens: 200},
		{InputTokens: 3000, OutputTokens: 1500, CacheCreationInputTokens: 800},
		{InputTokens: 250, OutputTokens: 4000},
	}
	var total Usage
	var sumOfParts float64
	for _, u := range turns {
		total.InputTokens += u.InputTokens
		total.OutputTokens += u.OutputTokens
		total.CacheReadInputTokens += u.CacheReadInputTokens
		total.CacheCreationInputTokens += u.CacheCreationInputTokens
		c, _ := CostUSD(model, u)
		sumOfParts += c
	}
	whole, _ := CostUSD(model, total)
	if !approxEqual(whole, sumOfParts) {
		t.Errorf("cost of summed usage = %v, sum of per-turn costs = %v; want equal", whole, sumOfParts)
	}
}

// budgetExceeded mirrors the loop's --max-budget-usd stop predicate
// (agent/loop.go: opts.MaxBudgetUSD > 0 && res.CostUSD >= opts.MaxBudgetUSD) so
// the decision to stop is covered here where the cost is computed.
func budgetExceeded(model string, u Usage, budget float64) bool {
	cost, _ := CostUSD(model, u)
	return budget > 0 && cost >= budget
}

func TestBudgetExceededPredicate(t *testing.T) {
	const model = "claude-opus-5" // output at $25/1M → 1000 output tokens = $0.025.
	const budget = 0.05

	// A cumulative tally that grows one turn at a time; the run should stop on
	// the turn where cost first reaches the budget.
	cumulative := []Usage{
		{OutputTokens: 1000}, // $0.025 — under
		{OutputTokens: 2000}, // $0.050 — at the limit, stop
		{OutputTokens: 3000}, // $0.075 — over (would already have stopped)
	}
	wantStop := []bool{false, true, true}
	for i, u := range cumulative {
		if got := budgetExceeded(model, u, budget); got != wantStop[i] {
			cost, _ := CostUSD(model, u)
			t.Errorf("turn %d: budgetExceeded(cost=%v, budget=%v) = %v, want %v", i, cost, budget, got, wantStop[i])
		}
	}

	// Budget 0 means unlimited: never stop, no matter how large the tally.
	if budgetExceeded(model, Usage{OutputTokens: 100_000_000}, 0) {
		t.Error("budget 0 must mean unlimited (never stop)")
	}

	// An unpriced model has cost 0, so a budget can never fire against it.
	if budgetExceeded("openai/gpt-oss-120b", Usage{OutputTokens: 100_000_000}, 0.01) {
		t.Error("unpriced model must never trip the budget (cost is 0)")
	}
}

func TestPricingLookup(t *testing.T) {
	if _, ok := Pricing("claude-opus-5"); !ok {
		t.Error("Pricing(claude-opus-5) should be known")
	}
	if _, ok := Pricing("opus"); !ok {
		t.Error("Pricing(opus) alias should resolve to a known price")
	}
	if p, ok := Pricing("no-such-model"); ok || p != (ModelPrice{}) {
		t.Errorf("Pricing(no-such-model) = %+v, ok=%v; want zero, false", p, ok)
	}
}
