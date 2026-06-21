package db

import "testing"

func TestEffectiveCPUMemGB(t *testing.T) {
	cases := []struct {
		raw    int
		strict bool
		want   int
	}{
		{0, false, 0},
		{0, true, 0},
		{-5, false, 0},
		{32, false, 32 + CPUMemHeadroomGB},
		{32, true, 32},
	}
	for _, c := range cases {
		if got := EffectiveCPUMemGB(c.raw, c.strict); got != c.want {
			t.Errorf("EffectiveCPUMemGB(%d, %v) = %d, want %d", c.raw, c.strict, got, c.want)
		}
	}
}

func TestRequestedCPUMemGB(t *testing.T) {
	mem := 64
	strict := true

	nonStrict := &Job{CLIResourceOverrides: &CLIResourceOverrides{CPUMemGB: &mem}}
	if got := nonStrict.RequestedCPUMemGB(); got != 64+CPUMemHeadroomGB {
		t.Errorf("non-strict = %d, want %d", got, 64+CPUMemHeadroomGB)
	}

	exact := &Job{CLIResourceOverrides: &CLIResourceOverrides{CPUMemGB: &mem, CPUMemStrict: &strict}}
	if got := exact.RequestedCPUMemGB(); got != 64 {
		t.Errorf("strict = %d, want 64", got)
	}

	if got := (&Job{}).RequestedCPUMemGB(); got != 0 {
		t.Errorf("unset = %d, want 0", got)
	}
}
