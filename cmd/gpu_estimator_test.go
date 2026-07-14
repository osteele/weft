package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/config"
)

func TestApplyGPUMemHeadroom(t *testing.T) {
	tests := []struct {
		name        string
		memGB       int
		hasExplicit bool
		strict      bool
		hardware    bool
		gpuClass    string
		want        int
	}{
		{name: "explicit non-strict adds headroom", memGB: 8, hasExplicit: true, strict: false, want: 10},
		{name: "explicit strict keeps exact", memGB: 8, hasExplicit: true, strict: true, want: 8},
		{name: "combined gpu selector keeps hardware floor exact", memGB: 80, hasExplicit: true, hardware: true, want: 80},
		{name: "non-explicit keeps value", memGB: 20, hasExplicit: false, strict: false, want: 20},
		{name: "known A100 80GB ceiling skips headroom", memGB: 80, hasExplicit: true, strict: false, gpuClass: "a100", want: 80},
		{name: "known H100 80GB ceiling skips headroom", memGB: 80, hasExplicit: true, strict: false, gpuClass: "h100", want: 80},
		{name: "floatable 24GB hardware bucket skips headroom", memGB: 24, hasExplicit: true, strict: false, gpuClass: "ampere+", want: 24},
		{name: "floatable 80GB hardware bucket skips headroom", memGB: 80, hasExplicit: true, strict: false, gpuClass: "nvidia", want: 80},
		{name: "floatable small request still gets headroom", memGB: 8, hasExplicit: true, strict: false, gpuClass: "ampere+", want: 10},
		{name: "below-ceiling A100 request still gets headroom", memGB: 50, hasExplicit: true, strict: false, gpuClass: "a100", want: 52},
		{name: "classless request still gets headroom", memGB: 24, hasExplicit: true, strict: false, gpuClass: "", want: 26},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyGPUMemHeadroom(tt.memGB, tt.hasExplicit, tt.strict, tt.hardware, tt.gpuClass); got != tt.want {
				t.Fatalf("applyGPUMemHeadroom(%d, explicit=%v, strict=%v, hardware=%v, class=%q) = %d, want %d",
					tt.memGB, tt.hasExplicit, tt.strict, tt.hardware, tt.gpuClass, got, tt.want)
			}
		})
	}
}

func TestResolveEffectiveGPUMemWithConfig_ExplicitHeadroomAndStrict(t *testing.T) {
	cfg := &config.Config{}
	explicit := 8

	floor, predicted := resolveEffectiveGPUMemWithConfig(cfg, &explicit, "", "nvidia", false, "", "", "", 0)
	if predicted {
		t.Fatalf("predicted = true, want false")
	}
	if floor == nil || *floor != 10 {
		t.Fatalf("floor = %v, want 10", floor)
	}

	floor, predicted = resolveEffectiveGPUMemWithConfig(cfg, &explicit, "", "nvidia", true, "", "", "", 0)
	if predicted {
		t.Fatalf("predicted = true, want false")
	}
	if floor == nil || *floor != 8 {
		t.Fatalf("floor = %v, want 8", floor)
	}
}
