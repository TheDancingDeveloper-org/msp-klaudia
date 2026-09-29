package api

// Cost tracking. This file is the single source of per-model USD pricing and
// the CostUSD helper that turns a token tally into a dollar figure. It mirrors
// the structure of the modelContextWindows / modelMaxOutputTokens tables in
// client.go: a per-model map keyed by the full model ID, looked up through
// ResolveModel so CLI aliases ("opus", "sonnet", …) price the same as the full
// snapshot IDs they resolve to.
//
// Rates are the first-party Anthropic Messages API list prices, in USD per one
// million tokens, as published on docs.claude.com — captured 2026-06-24. They
// are list prices and WILL drift: when Anthropic reprices a model or ships a
// new one, update the table below. Bedrock / Vertex / Foundry bill separately;
// this table does not attempt to model partner pricing.
//
// Cache rates follow Anthropic's standard multipliers rather than being quoted
// independently, because the model pages express them that way: a 5-minute
// cache WRITE costs 1.25× the base input rate, and a cache READ costs 0.10× the
// base input rate. We store all four rates explicitly (not as runtime
// multipliers) so the table is auditable at a glance and a future per-model
// exception (some newer models publish a flat cache-read price) is a one-line
// edit rather than a special case in the arithmetic.

// ModelPrice is a per-model price sheet in USD per 1,000,000 tokens.
type ModelPrice struct {
	Input      float64 // uncached input tokens
	Output     float64 // generated output tokens
	CacheRead  float64 // input tokens served from the prompt cache
	CacheWrite float64 // input tokens written to the prompt cache (5-minute TTL)
}

// Usage is the token tally CostUSD prices. It is a small local mirror of the
// four fields carried on the SDK's BetaUsage and on agent.Result, so the api
// package need not import agent (which would be a cycle) and callers can build
// it from whichever usage source they hold.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// perMillion is the number of tokens a per-1M rate is quoted against.
const perMillion = 1_000_000.0

// modelPricing maps full model IDs to their price sheet. Keys match the IDs in
// modelContextWindows / modelMaxOutputTokens (client.go). A model absent here
// is priced at zero and reported as unknown by CostUSD — that is the correct
// behaviour for OpenAI-compatible endpoints and any model whose rate we have
// not verified, so an unknown model never fabricates a dollar figure or trips
// a budget stop.
//
// Cache columns are the standard multiples of the input column
// (read = 0.10×, write = 1.25×), pre-computed for auditability.
var modelPricing = map[string]ModelPrice{
	// Claude Opus 5 — $5 / $25.
	"claude-opus-5": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	// Claude Sonnet 5 — $2 / $10.
	"claude-sonnet-5": {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2.50},
	// Claude Fable 5 — $10 / $50 (Anthropic's most capable tier).
	"claude-fable-5": {Input: 10, Output: 50, CacheRead: 1.00, CacheWrite: 12.50},
	// Claude Opus 4.x — $5 / $25 across 4.6, 4.7, 4.8.
	"claude-opus-4-8": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-opus-4-7": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-opus-4-6": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	// Claude Opus 4.5 — $5 / $25.
	"claude-opus-4-5-20251101": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	// Claude Sonnet 4.6 / 4.5 — $3 / $15.
	"claude-sonnet-4-6":          {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	"claude-sonnet-4-5-20250929": {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	// Claude Haiku 4.5 — $1 / $5.
	"claude-haiku-4-5":          {Input: 1, Output: 5, CacheRead: 0.10, CacheWrite: 1.25},
	"claude-haiku-4-5-20251001": {Input: 1, Output: 5, CacheRead: 0.10, CacheWrite: 1.25},
}

// Pricing returns the price sheet for a model, resolving aliases the way
// ContextWindow / MaxOutputTokens do. ok is false when the model has no known
// rate (its zero-value ModelPrice is returned).
func Pricing(model string) (price ModelPrice, ok bool) {
	price, ok = modelPricing[string(ResolveModel(model))]
	return price, ok
}

// CostUSD computes the cumulative cost in USD of a token tally for a model.
// known is false when the model has no entry in modelPricing; in that case the
// cost is 0 (we do not guess a rate). Callers use known to render "unknown"
// rather than a misleading "$0.00", and the loop uses it implicitly: a
// --max-budget-usd stop can never fire for an unpriced model because its cost
// stays 0.
func CostUSD(model string, u Usage) (usd float64, known bool) {
	price, ok := modelPricing[string(ResolveModel(model))]
	if !ok {
		return 0, false
	}
	usd = float64(u.InputTokens)/perMillion*price.Input +
		float64(u.OutputTokens)/perMillion*price.Output +
		float64(u.CacheReadInputTokens)/perMillion*price.CacheRead +
		float64(u.CacheCreationInputTokens)/perMillion*price.CacheWrite
	return usd, true
}
