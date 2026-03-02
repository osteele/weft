package placement

import "testing"

func TestGenerationOf(t *testing.T) {
	tests := []struct {
		class string
		want  GPUGeneration
	}{
		{"a100", GenAmpere},
		{"rtx3090", GenAmpere},
		{"rtx2080ti", GenTuring},
		{"h100", GenHopper},
		{"rtx4090", GenAdaLovelace},
		{"m2max", GenAppleM2},
		{"b200", GenBlackwell},
		{"unknown", GenUnknown},
	}
	for _, tt := range tests {
		got := generationOf(tt.class)
		if got != tt.want {
			t.Errorf("generationOf(%q) = %d, want %d", tt.class, got, tt.want)
		}
	}
}

func TestGenerationOrdering(t *testing.T) {
	if GenTuring >= GenAmpere {
		t.Error("Turing should be older than Ampere")
	}
	if GenAmpere >= GenAdaLovelace {
		t.Error("Ampere should be older than Ada Lovelace")
	}
	if GenAdaLovelace >= GenHopper {
		t.Error("Ada Lovelace should be older than Hopper")
	}
	if GenHopper >= GenBlackwell {
		t.Error("Hopper should be older than Blackwell")
	}
	if GenAppleM1 >= GenAppleM2 {
		t.Error("M1 should be older than M2")
	}
	if GenAppleM2 >= GenAppleM3 {
		t.Error("M2 should be older than M3")
	}
	if GenAppleM3 >= GenAppleM4 {
		t.Error("M3 should be older than M4")
	}
}

func TestParseGPUConstraint(t *testing.T) {
	tests := []struct {
		input string
		mode  gpuConstraintMode
		gen   GPUGeneration
		norm  string
	}{
		// Exact model
		{"a100", constraintExactModel, GenUnknown, "a100"},
		{"rtx3090", constraintExactModel, GenUnknown, "rtx3090"},
		{"RTX 3090", constraintExactModel, GenUnknown, "rtx3090"},
		{"unknown-gpu", constraintExactModel, GenUnknown, "unknowngpu"},

		// Exact generation
		{"ampere", constraintExactGen, GenAmpere, "ampere"},
		{"turing", constraintExactGen, GenTuring, "turing"},
		{"ada", constraintExactGen, GenAdaLovelace, "ada"},
		{"hopper", constraintExactGen, GenHopper, "hopper"},

		// Minimum generation (from generation name)
		{"ampere+", constraintMinGen, GenAmpere, "ampere"},
		{"turing+", constraintMinGen, GenTuring, "turing"},

		// Minimum generation (promoted from model name)
		{"a100+", constraintMinGen, GenAmpere, "a100"},
		{"rtx3090+", constraintMinGen, GenAmpere, "rtx3090"},
		{"h100+", constraintMinGen, GenHopper, "h100"},

		// Unknown model with '+' falls back to exact model
		{"unknowngpu+", constraintExactModel, GenUnknown, "unknowngpu"},
	}

	for _, tt := range tests {
		c := parseGPUConstraint(tt.input)
		if c.mode != tt.mode {
			t.Errorf("parseGPUConstraint(%q).mode = %d, want %d", tt.input, c.mode, tt.mode)
		}
		if c.mode != constraintExactModel && c.generation != tt.gen {
			t.Errorf("parseGPUConstraint(%q).generation = %d, want %d", tt.input, c.generation, tt.gen)
		}
		if c.normalized != tt.norm {
			t.Errorf("parseGPUConstraint(%q).normalized = %q, want %q", tt.input, c.normalized, tt.norm)
		}
	}
}

func TestMatchesGPU(t *testing.T) {
	tests := []struct {
		constraint string
		inventory  string
		want       bool
	}{
		// Exact model matches
		{"a100", "a100", true},
		{"a100", "rtx3090", false},
		{"rtx3090", "rtx3090", true},
		{"a100", "m2max", false},

		// Exact generation matches
		{"ampere", "a100", true},
		{"ampere", "rtx3090", true},
		{"ampere", "rtx2080ti", false},
		{"ampere", "m2max", false},
		{"turing", "rtx2080ti", true},
		{"turing", "a100", false},

		// Minimum generation matches
		{"ampere+", "a100", true},
		{"ampere+", "rtx3090", true},
		{"ampere+", "rtx4090", true},    // Ada Lovelace > Ampere
		{"ampere+", "h100", true},       // Hopper > Ampere
		{"ampere+", "rtx2080ti", false}, // Turing < Ampere
		{"ampere+", "m2max", false},     // Cross-family blocked

		// Model promoted to generation with '+'
		{"a100+", "a100", true},
		{"a100+", "rtx3090", true},    // Both Ampere
		{"a100+", "h100", true},       // Hopper > Ampere
		{"a100+", "rtx2080ti", false}, // Turing < Ampere

		// Hopper minimum
		{"hopper+", "h100", true},
		{"hopper+", "b200", true},     // Blackwell > Hopper
		{"hopper+", "a100", false},    // Ampere < Hopper
		{"hopper+", "rtx4090", false}, // Ada < Hopper

		// Apple cross-family blocked
		{"ampere+", "m1max", false},

		// Apple generation matching
		{"applem2", "m2max", true},   // exact Apple generation
		{"applem2+", "m2max", true},  // M2 >= M2
		{"applem2+", "m3max", true},  // M3 > M2
		{"applem2+", "m1max", false}, // M1 < M2
		{"applem3+", "m2max", false}, // M2 < M3

		// Unknown inventory class
		{"ampere+", "somethingweird", false},
		{"ampere", "somethingweird", false},
	}

	for _, tt := range tests {
		c := parseGPUConstraint(tt.constraint)
		got := c.matchesGPU(tt.inventory)
		if got != tt.want {
			t.Errorf("parseGPUConstraint(%q).matchesGPU(%q) = %v, want %v", tt.constraint, tt.inventory, got, tt.want)
		}
	}
}
