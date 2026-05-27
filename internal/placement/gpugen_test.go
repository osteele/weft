package placement

import "testing"

func TestGenerationOf(t *testing.T) {
	tests := []struct {
		class string
		want  GPUGeneration
	}{
		{"v100", GenVolta},
		{"a100", GenAmpere},
		{"a5000", GenAmpere},
		{"rtxa5000", GenAmpere},
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
	if GenVolta >= GenTuring {
		t.Error("Volta should be older than Turing")
	}
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
		{"volta", constraintExactGen, GenVolta, "volta"},
		{"turing", constraintExactGen, GenTuring, "turing"},
		{"ada", constraintExactGen, GenAdaLovelace, "ada"},
		{"hopper", constraintExactGen, GenHopper, "hopper"},

		// Minimum generation (from generation name)
		{"ampere+", constraintMinGen, GenAmpere, "ampere"},
		{"volta+", constraintMinGen, GenVolta, "volta"},
		{"turing+", constraintMinGen, GenTuring, "turing"},

		// Minimum generation (promoted from model name)
		{"a100+", constraintMinGen, GenAmpere, "a100"},
		{"v100+", constraintMinGen, GenVolta, "v100"},
		{"rtx3090+", constraintMinGen, GenAmpere, "rtx3090"},
		{"h100+", constraintMinGen, GenHopper, "h100"},
		{"a5000+", constraintMinGen, GenAmpere, "a5000"},

		// Family matching
		{"nvidia", constraintFamily, GenUnknown, "nvidia"},
		{"nvidia+", constraintFamily, GenUnknown, "nvidia"},
		{"apple", constraintFamily, GenUnknown, "apple"},
		{"NVIDIA", constraintFamily, GenUnknown, "nvidia"},

		// Unknown model with '+' falls back to exact model
		{"unknowngpu+", constraintExactModel, GenUnknown, "unknowngpu"},
	}

	for _, tt := range tests {
		c := ParseGPUConstraint(tt.input)
		if c.mode != tt.mode {
			t.Errorf("ParseGPUConstraint(%q).mode = %d, want %d", tt.input, c.mode, tt.mode)
		}
		if c.mode != constraintExactModel && c.generation != tt.gen {
			t.Errorf("ParseGPUConstraint(%q).generation = %d, want %d", tt.input, c.generation, tt.gen)
		}
		if c.normalized != tt.norm {
			t.Errorf("ParseGPUConstraint(%q).normalized = %q, want %q", tt.input, c.normalized, tt.norm)
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
		{"volta+", "v100", true},
		{"volta+", "rtx2080ti", true}, // Turing > Volta
		{"ampere+", "m2max", false},   // Cross-family blocked

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

		// Family matching — nvidia
		{"nvidia", "a100", true},
		{"nvidia", "rtx3090", true},
		{"nvidia", "rtx2080ti", true},
		{"nvidia", "h100", true},
		{"nvidia", "rtx4090", true},
		{"nvidia", "m2max", false},
		{"nvidia", "m1pro", false},

		// Family matching — apple
		{"apple", "m2max", true},
		{"apple", "m1pro", true},
		{"apple", "m3max", true},
		{"apple", "m4", true},
		{"apple", "a100", false},
		{"apple", "rtx3090", false},

		// Unknown inventory class
		{"ampere+", "somethingweird", false},
		{"ampere", "somethingweird", false},
		{"nvidia", "somethingweird", false},
	}

	for _, tt := range tests {
		c := ParseGPUConstraint(tt.constraint)
		got := c.MatchesGPU(tt.inventory)
		if got != tt.want {
			t.Errorf("ParseGPUConstraint(%q).MatchesGPU(%q) = %v, want %v", tt.constraint, tt.inventory, got, tt.want)
		}
	}
}

func TestMatchesGPUFullName(t *testing.T) {
	tests := []struct {
		constraint string
		fullName   string
		want       bool
	}{
		// Exact model — substring match on normalized full name
		{"a100", "NVIDIA A100-PCIE-80GB", true},
		{"a100", "NVIDIA GeForce RTX 3090", false},
		{"rtx3090", "NVIDIA GeForce RTX 3090", true},
		{"3090", "NVIDIA GeForce RTX 3090", true},

		// Generation — identify model from full name
		{"ampere", "NVIDIA A100-PCIE-80GB", true},
		{"ampere", "NVIDIA GeForce RTX 3090", true},
		{"ampere", "NVIDIA GeForce RTX 2080 Ti", false},
		{"turing", "NVIDIA GeForce RTX 2080 Ti", true},
		{"turing", "NVIDIA A100-PCIE-80GB", false},
		{"volta", "Tesla V100-SXM2-16GB", true},
		{"volta", "NVIDIA GeForce RTX 2080 Ti", false},

		// Minimum generation
		{"ampere+", "NVIDIA A100-PCIE-80GB", true},
		{"ampere+", "NVIDIA GeForce RTX 3090", true},
		{"ampere+", "NVIDIA GeForce RTX 2080 Ti", false}, // Turing < Ampere
		{"turing+", "NVIDIA GeForce RTX 2080 Ti", true},
		{"turing+", "NVIDIA A100-PCIE-80GB", true},   // Ampere > Turing
		{"turing+", "NVIDIA GeForce RTX 3090", true}, // Ampere > Turing
		{"hopper+", "NVIDIA A100-PCIE-80GB", false},  // Ampere < Hopper

		// Family matching on full names
		{"nvidia", "NVIDIA A100-PCIE-80GB", true},
		{"nvidia", "NVIDIA GeForce RTX 3090", true},
		{"nvidia", "NVIDIA GeForce RTX 2080 Ti", true},
		{"apple", "NVIDIA A100-PCIE-80GB", false},

		// Truncated names from nvidia-smi on old drivers (525.x)
		{"nvidia", "NVIDIA GeForce ...", true},
		{"apple", "NVIDIA GeForce ...", false},
		{"ampere", "NVIDIA GeForce ...", false}, // can't determine generation from truncated name

		// Unknown full name
		{"ampere+", "Some Unknown GPU", false},
		{"ampere", "Some Unknown GPU", false},
		{"nvidia", "Some Unknown GPU", false},
	}

	for _, tt := range tests {
		c := ParseGPUConstraint(tt.constraint)
		got := c.MatchesGPUFullName(tt.fullName)
		if got != tt.want {
			t.Errorf("ParseGPUConstraint(%q).MatchesGPUFullName(%q) = %v, want %v",
				tt.constraint, tt.fullName, got, tt.want)
		}
	}
}

func TestSubsumes(t *testing.T) {
	tests := []struct {
		broader  string
		narrower string
		want     bool
	}{
		// Family subsumes generations and models
		{"nvidia", "ampere", true},
		{"nvidia", "turing", true},
		{"nvidia", "hopper+", true},
		{"nvidia", "a100", true},
		{"nvidia", "rtx3090", true},
		{"nvidia", "nvidia", true},
		{"nvidia", "apple", false},
		{"nvidia", "m2max", false},
		{"apple", "m2max", true},
		{"apple", "applem2", true},
		{"apple", "nvidia", false},

		// MinGen subsumes higher minGen, same/higher exactGen, models in range
		{"ampere+", "hopper+", true},
		{"ampere+", "ampere+", true},
		{"ampere+", "turing+", false}, // turing+ includes turing, which ampere+ doesn't
		{"ampere+", "hopper", true},
		{"ampere+", "ampere", true},
		{"ampere+", "turing", false},
		{"ampere+", "h100", true},
		{"ampere+", "a100", true},
		{"ampere+", "rtx2080ti", false},
		{"hopper+", "ampere+", false},
		{"hopper+", "blackwell", true},
		{"hopper+", "h100", true},
		{"hopper+", "a100", false},

		// ExactGen subsumes models within that generation
		{"ampere", "a100", true},
		{"ampere", "rtx3090", true},
		{"ampere", "h100", false},
		{"ampere", "rtx2080ti", false},
		{"ampere", "ampere", true},
		{"ampere", "hopper", false},
		{"ampere", "nvidia", false},
		{"ampere", "ampere+", false},

		// ExactModel subsumes only itself
		{"a100", "a100", true},
		{"a100", "rtx3090", false},
		{"a100", "ampere", false},
		{"a100", "nvidia", false},

		// Cross-family never subsumes
		{"ampere+", "m2max", false},
		{"applem2+", "a100", false},
	}

	for _, tt := range tests {
		c := ParseGPUConstraint(tt.broader)
		other := ParseGPUConstraint(tt.narrower)
		got := c.Subsumes(other)
		if got != tt.want {
			t.Errorf("ParseGPUConstraint(%q).Subsumes(ParseGPUConstraint(%q)) = %v, want %v",
				tt.broader, tt.narrower, got, tt.want)
		}
	}
}

func TestMinCUDAForGPU(t *testing.T) {
	tests := []struct {
		gpuName string
		want    float64
	}{
		{"RTX 5090", 12.8},
		{"Tesla V100", 9.0},
		{"V100", 9.0},
		{"RTX 4090", 11.8},
		{"RTX 3090", 11.0},
		{"RTX A5000", 11.0},
		{"A5000", 11.0},
		{"A100 SXM4", 11.0},
		{"H100", 12.0},
		{"H200", 12.0},
		{"B200", 12.8},
		{"Unknown GPU", 0},
	}
	for _, tt := range tests {
		got := MinCUDAForGPU(tt.gpuName)
		if got != tt.want {
			t.Errorf("MinCUDAForGPU(%q) = %v, want %v", tt.gpuName, got, tt.want)
		}
	}
}

func TestMinCUDAForConstraint(t *testing.T) {
	tests := []struct {
		gpuClass string
		want     float64
	}{
		{"RTX-5090", 12.8},
		{"volta", 9.0},
		{"blackwell", 12.8},
		{"blackwell+", 12.8},
		{"a100+", 11.0},
		{"ampere", 11.0},
		{"nvidia", 12.8},
		{"apple", 0},
		{"", 0},
		{"unknown-model", 0},
	}
	for _, tt := range tests {
		got := MinCUDAForConstraint(tt.gpuClass)
		if got != tt.want {
			t.Errorf("MinCUDAForConstraint(%q) = %v, want %v", tt.gpuClass, got, tt.want)
		}
	}
}

func TestParseCUDADriverFloor(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		// Empty / sentinel
		{"", "", false},
		{"any", "", false},
		{"none", "", false},
		// Numeric versions
		{"12", "12.0", false},
		{"12.4", "12.4", false},
		{"12.8", "12.8", false},
		{"12.8.1", "12.8", false},
		{"  12.4  ", "12.4", false},
		// Generation names
		{"hopper", "12.0", false},
		{"Hopper", "12.0", false},
		{"blackwell", "12.8", false},
		{"ada", "11.8", false},
		{"adalovelace", "11.8", false},
		{"ampere", "11.0", false},
		{"turing", "10.0", false},
		// Wheel tags
		{"cu128", "12.8", false},
		{"cu121", "12.1", false},
		{"cu118", "11.8", false},
		// Invalid
		{"hopperish", "", true},
		{"12.x", "", true},
		{"abc", "", true},
	}
	for _, tt := range tests {
		got, err := ParseCUDADriverFloor(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseCUDADriverFloor(%q) err=%v, wantErr=%v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseCUDADriverFloor(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
