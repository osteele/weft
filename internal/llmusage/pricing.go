package llmusage

import "strings"

// Pricing is a per-million-token price tuple in USD.
//
// All four token classes are billed separately by Anthropic:
//   - InputUSDPerM       — uncached input tokens
//   - OutputUSDPerM      — output tokens
//   - CacheWriteUSDPerM  — input tokens written to the prompt cache
//     (typically 1.25× input price)
//   - CacheReadUSDPerM   — input tokens served from the prompt cache
//     (typically 0.1× input price)
//
// Values are taken from Anthropic's public pricing page as of 2026-05-29.
// Update whenever Anthropic publishes new prices; the same table is read at
// every recording-time cost computation, so a change only affects new calls.
type Pricing struct {
	InputUSDPerM      float64
	OutputUSDPerM     float64
	CacheWriteUSDPerM float64
	CacheReadUSDPerM  float64
}

// anthropicPricing maps Anthropic model IDs to their per-million-token
// prices. Lookup is case-insensitive with a longest-prefix fallback so
// model strings like "claude-opus-4-7-20260315" still resolve.
//
// Sources:
//   - https://www.anthropic.com/pricing (Claude 4 family rates)
//   - https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
//     (cache write 1.25× input; cache read 0.1× input)
var anthropicPricing = map[string]Pricing{
	// Claude 4.x family. Opus is premium; Sonnet mid-tier; Haiku cheap.
	"claude-opus-4-7":   {InputUSDPerM: 15.00, OutputUSDPerM: 75.00, CacheWriteUSDPerM: 18.75, CacheReadUSDPerM: 1.50},
	"claude-opus-4-6":   {InputUSDPerM: 15.00, OutputUSDPerM: 75.00, CacheWriteUSDPerM: 18.75, CacheReadUSDPerM: 1.50},
	"claude-opus-4-5":   {InputUSDPerM: 15.00, OutputUSDPerM: 75.00, CacheWriteUSDPerM: 18.75, CacheReadUSDPerM: 1.50},
	"claude-sonnet-4-6": {InputUSDPerM: 3.00, OutputUSDPerM: 15.00, CacheWriteUSDPerM: 3.75, CacheReadUSDPerM: 0.30},
	"claude-sonnet-4-5": {InputUSDPerM: 3.00, OutputUSDPerM: 15.00, CacheWriteUSDPerM: 3.75, CacheReadUSDPerM: 0.30},
	"claude-haiku-4-5":  {InputUSDPerM: 1.00, OutputUSDPerM: 5.00, CacheWriteUSDPerM: 1.25, CacheReadUSDPerM: 0.10},
}

// PriceForModel returns the pricing tuple for a model ID, or the zero value
// (cost will be 0) if the model is unknown. Unknown-model recording is still
// useful because token counts and call counts remain accurate.
func PriceForModel(model string) Pricing {
	m := strings.ToLower(strings.TrimSpace(model))
	if p, ok := anthropicPricing[m]; ok {
		return p
	}
	// Longest-prefix fallback: many real model strings are versioned
	// suffixes of the canonical name (e.g. "claude-opus-4-7-20260315").
	var bestKey string
	for k := range anthropicPricing {
		if strings.HasPrefix(m, k) && len(k) > len(bestKey) {
			bestKey = k
		}
	}
	if bestKey != "" {
		return anthropicPricing[bestKey]
	}
	return Pricing{}
}

// CostMicros computes the call cost in micro-dollars (1e-6 USD) given a
// Pricing tuple and token counts.
func CostMicros(p Pricing, inputTok, outputTok, cacheWriteTok, cacheReadTok int) int64 {
	usd := p.InputUSDPerM*float64(inputTok) +
		p.OutputUSDPerM*float64(outputTok) +
		p.CacheWriteUSDPerM*float64(cacheWriteTok) +
		p.CacheReadUSDPerM*float64(cacheReadTok)
	// usd is dollars-per-million-tokens × tokens, so divide by 1e6 to get
	// dollars, then multiply by 1e6 again to get micros. The two cancel.
	return int64(usd)
}
