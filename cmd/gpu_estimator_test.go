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
		want        int
	}{
		{name: "explicit non-strict adds headroom", memGB: 8, hasExplicit: true, strict: false, want: 10},
		{name: "explicit strict keeps exact", memGB: 8, hasExplicit: true, strict: true, want: 8},
		{name: "non-explicit keeps value", memGB: 20, hasExplicit: false, strict: false, want: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyGPUMemHeadroom(tt.memGB, tt.hasExplicit, tt.strict); got != tt.want {
				t.Fatalf("applyGPUMemHeadroom(%d, explicit=%v, strict=%v) = %d, want %d",
					tt.memGB, tt.hasExplicit, tt.strict, got, tt.want)
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
