package llmusage

import "testing"

func TestPriceForModel(t *testing.T) {
	cases := []struct {
		model       string
		wantInput   float64
		wantCacheRd float64
	}{
		{"claude-opus-4-7", 15.00, 1.50},
		{"CLAUDE-OPUS-4-7", 15.00, 1.50},          // case-insensitive
		{"claude-opus-4-7-20260315", 15.00, 1.50}, // versioned suffix
		{"claude-sonnet-4-6", 3.00, 0.30},
		{"claude-haiku-4-5", 1.00, 0.10},
		{"unknown-model", 0, 0},
	}
	for _, tc := range cases {
		p := PriceForModel(tc.model)
		if p.InputUSDPerM != tc.wantInput {
			t.Errorf("PriceForModel(%q).InputUSDPerM = %v, want %v", tc.model, p.InputUSDPerM, tc.wantInput)
		}
		if p.CacheReadUSDPerM != tc.wantCacheRd {
			t.Errorf("PriceForModel(%q).CacheReadUSDPerM = %v, want %v", tc.model, p.CacheReadUSDPerM, tc.wantCacheRd)
		}
	}
}

func TestCostMicrosOpusExample(t *testing.T) {
	// 1k input, 500 output on Opus 4.7:
	//   input:  15.00 USD/M  * 1000 / 1M  = 0.015 USD = 15000 micros
	//   output: 75.00 USD/M  * 500  / 1M  = 0.0375 USD = 37500 micros
	// However the implementation skips the /1e6 step and multiplies by 1e6,
	// so the raw computation yields the same micro value.
	p := PriceForModel("claude-opus-4-7")
	got := CostMicros(p, 1000, 500, 0, 0)
	want := int64(15.00*1000 + 75.00*500) // 15000 + 37500 = 52500
	if got != want {
		t.Errorf("CostMicros = %d, want %d", got, want)
	}
}

func TestRecorderNilSafe(t *testing.T) {
	r := NewRecorder(nil)
	if err := r.Record(Call{Provider: "anthropic", Model: "claude-opus-4-7", Feature: "narrate"}); err != nil {
		t.Fatalf("nil recorder should be a no-op, got %v", err)
	}
}
