package agent

import (
	"math"
	"testing"
	"time"
)

func TestGeminiPriceAt_DateSwitch(t *testing.T) {
	cases := []struct {
		model            string
		at               time.Time
		in, out, cacheRd float64
	}{
		{"gemini-4-argon", time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), 2, 10, 0.10},
		{"gemini-4-argon", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), 4, 20, 0.20},
		{"gemini-3.8-flash", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), 0.75, 3.75, 0.075},
		{"gemini-3.8-flash", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1.50, 7.50, 0.15},
	}
	for _, c := range cases {
		p, ok := geminiPriceAt(c.model, c.at)
		if !ok || p.input != c.in || p.output != c.out || p.cacheRead != c.cacheRd {
			t.Errorf("%s @ %s = %+v ok=%v", c.model, c.at, p, ok)
		}
	}
	for _, m := range []string{"flash", "pro", "auto", "gemini-3-flash-base", ""} {
		if _, ok := geminiPriceAt(m, time.Now()); ok {
			t.Errorf("geminiPriceAt(%q) should be unknown", m)
		}
	}
}

func TestGeminiCostUSD(t *testing.T) {
	intro := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	argon := geminiModelStats{TotalTokens: 9212, InputTokens: 9103, OutputTokens: 6, Cached: 1000}
	// input unset → prompt − cached = 8103; thoughts = 9212−9103−6 = 103.
	want := (8103*2.0 + 1000*0.10 + 109*10.0) / 1e6
	if got := geminiCostUSD(map[string]geminiModelStats{"gemini-4-argon": argon}, intro); math.Abs(got-want) > 1e-12 {
		t.Fatalf("argon intro = %v, want %v", got, want)
	}
	if got := geminiCostUSD(map[string]geminiModelStats{"gemini-4-argon": argon}, later); math.Abs(got-2*want) > 1e-12 {
		t.Fatalf("argon after step-up = %v, want %v", got, 2*want)
	}

	// Two priced models sum.
	flash := geminiModelStats{TotalTokens: 300, InputTokens: 200, OutputTokens: 100}
	both := map[string]geminiModelStats{"gemini-4-argon": argon, "gemini-3.8-flash": flash}
	if got := geminiCostUSD(both, intro); math.Abs(got-(want+(200*0.75+100*3.75)/1e6)) > 1e-12 {
		t.Fatalf("two models = %v", got)
	}

	// Unknown model with tokens → unset; unknown model with zero usage is ignored.
	if got := geminiCostUSD(map[string]geminiModelStats{"gemini-4-argon": argon, "gemini-3-flash-base": flash}, intro); got != 0 {
		t.Fatalf("unknown model priced: %v", got)
	}
	if got := geminiCostUSD(map[string]geminiModelStats{"gemini-4-argon": argon, "mystery": {}}, intro); math.Abs(got-want) > 1e-12 {
		t.Fatalf("zero-usage unknown model should be ignored: %v", got)
	}
	if got := geminiCostUSD(nil, intro); got != 0 {
		t.Fatalf("nil models = %v", got)
	}
	// total < input+output (malformed) clamps thoughts at 0.
	odd := geminiModelStats{TotalTokens: 1, InputTokens: 100, OutputTokens: 10, Input: new(int)}
	if got := geminiCostUSD(map[string]geminiModelStats{"gemini-4-argon": odd}, intro); math.Abs(got-(10*10.0)/1e6) > 1e-12 {
		t.Fatalf("clamped = %v", got)
	}
}
