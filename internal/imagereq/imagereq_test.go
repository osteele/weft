package imagereq

import "testing"

func TestParseNVIDIARequireCUDA(t *testing.T) {
	req := ParseNVIDIARequireCUDA(`cuda>=12.9 brand=tesla,driver>=535,driver<536 brand=tesla,driver>=550,driver<551`)
	if req.MinCUDAVersion != "12.9" {
		t.Fatalf("MinCUDAVersion = %q, want 12.9", req.MinCUDAVersion)
	}
	if req.MinDriverVersion != 535 {
		t.Fatalf("MinDriverVersion = %d, want 535", req.MinDriverVersion)
	}
}

func TestFromEnv(t *testing.T) {
	req := FromEnv([]string{
		"PATH=/usr/bin",
		"NVIDIA_REQUIRE_CUDA=cuda>=12.4 driver>=525",
	})
	if req.MinCUDAVersion != "12.4" || req.MinDriverVersion != 525 {
		t.Fatalf("requirements = %+v, want CUDA 12.4 driver 525", req)
	}
}

func TestExplicit(t *testing.T) {
	req, err := Explicit("535", "12.8")
	if err != nil {
		t.Fatal(err)
	}
	if req.MinDriverVersion != 535 || req.MinCUDAVersion != "12.8" {
		t.Fatalf("requirements = %+v", req)
	}
}
