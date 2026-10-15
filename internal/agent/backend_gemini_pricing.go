package agent

import "time"

// Gemini API pricing (Paid tier, "Standard" tab), USD per 1M tokens.
//
// Source: https://ai.google.dev/gemini-api/docs/pricing (last updated
// 2026-10-14), plus the model pages
// https://ai.google.dev/gemini-api/docs/generate-content/latest-model
// (Gemini 4 Argon) and
// https://ai.google.dev/gemini-api/docs/generate-content/whats-new-gemini-3.8
// (gemini-3.8-flash). Output rates include thinking tokens. Neither model
// has a long-context tier on the Standard tab, so one rate applies to the
// whole request.
//
//	gemini-4-argon   until 2026-11-30: in $2.00 / out $10.00 / cached $0.10
//	                 from  2026-12-01: in $4.00 / out $20.00 / cached $0.20
//	gemini-3.8-flash until 2026-12-31: in $0.75 / out $3.75  / cached $0.075
//	                 from  2027-01-01: in $1.50 / out $7.50  / cached $0.15
//
// The step-up dates are the introductory-price end dates on the pricing
// page; kojo switches on the turn's completion time (UTC). Batch / Flex /
// Priority tiers and cache storage are not used by gemini-cli and are not
// modelled.
type geminiPriceStep struct {
	from                     time.Time // inclusive; zero = since forever
	input, output, cacheRead float64   // USD per 1M tokens
}

var geminiPriceTable = map[string][]geminiPriceStep{
	"gemini-4-argon": {
		{input: 2.00, output: 10.00, cacheRead: 0.10},
		{from: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), input: 4.00, output: 20.00, cacheRead: 0.20},
	},
	"gemini-3.8-flash": {
		{input: 0.75, output: 3.75, cacheRead: 0.075},
		{from: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), input: 1.50, output: 7.50, cacheRead: 0.15},
	},
}

// geminiPriceAt returns the price step for model at time at.
func geminiPriceAt(model string, at time.Time) (geminiPriceStep, bool) {
	steps, ok := geminiPriceTable[model]
	if !ok || len(steps) == 0 {
		return geminiPriceStep{}, false
	}
	cur := steps[0]
	for _, s := range steps[1:] {
		if !at.Before(s.from) {
			cur = s
		}
	}
	return cur, true
}

// geminiModelStats is one entry of the stream-json result's
// stats.models map. gemini-cli fills it from the API's usageMetadata:
// input_tokens = promptTokenCount, output_tokens = candidatesTokenCount
// (thoughts EXCLUDED), cached = cachedContentTokenCount, input =
// prompt − cached, total_tokens = totalTokenCount (prompt + candidates +
// thoughts + toolUsePrompt). The stream has no thoughts field, so thoughts
// are derived as total − input_tokens − output_tokens.
type geminiModelStats struct {
	TotalTokens  int  `json:"total_tokens"`
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	Cached       int  `json:"cached"`
	Input        *int `json:"input"`
}

// uncached returns the non-cached prompt tokens.
func (s geminiModelStats) uncached() int {
	n := s.InputTokens - s.Cached
	if s.Input != nil {
		n = *s.Input
	}
	if n < 0 {
		n = 0
	}
	return n
}

// thoughts returns the thinking tokens implied by total_tokens. Any
// toolUsePromptTokenCount (server-side tools only; gemini-cli's own
// function calls are client-side and report 0) lands here too and is
// billed at the output rate, a slight overestimate in that rare case.
func (s geminiModelStats) thoughts() int {
	n := s.TotalTokens - s.InputTokens - s.OutputTokens
	if n < 0 {
		n = 0
	}
	return n
}

// geminiCostUSD prices one invocation from the per-model stats. Returns 0
// (cost unset → the UI shows none) when there are no per-model stats or
// when any model that consumed tokens has no known price: a partial sum
// would be presented as the turn's real cost.
func geminiCostUSD(models map[string]geminiModelStats, at time.Time) float64 {
	if len(models) == 0 {
		return 0
	}
	var total float64
	for model, s := range models {
		if s.TotalTokens == 0 && s.InputTokens == 0 && s.OutputTokens == 0 {
			continue
		}
		p, ok := geminiPriceAt(model, at)
		if !ok {
			return 0
		}
		total += float64(s.uncached())*p.input/1e6 +
			float64(s.Cached)*p.cacheRead/1e6 +
			float64(s.OutputTokens+s.thoughts())*p.output/1e6
	}
	return total
}
