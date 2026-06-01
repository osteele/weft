package imagereq

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

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

func TestMinDriverForCUDA(t *testing.T) {
	cases := []struct {
		cuda string
		want int
	}{
		{"12.0", 525},
		{"12.1", 530}, // torch 2.0-2.3's cu121 wheel
		{"12.2", 535},
		{"12.3", 545},
		{"12.4", 550},
		{"12.5", 555},
		{"12.6", 560},
		{"12.7", 560}, // 12.7 was skipped publicly; floor to 12.6
		{"12.8", 570},
		{"13.0", 580},
		{"11.8", 0}, // below lowest tabled
		{"", 0},
		{"garbage", 0},
	}
	for _, tc := range cases {
		if got := MinDriverForCUDA(tc.cuda); got != tc.want {
			t.Errorf("MinDriverForCUDA(%q) = %d, want %d", tc.cuda, got, tc.want)
		}
	}
}

func TestBackfillDriverFromCUDA(t *testing.T) {
	t.Run("backfills driver when zero", func(t *testing.T) {
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{MinCUDAVersion: "12.8"})
		if out.MinDriverVersion != 570 || out.MinCUDAVersion != "12.8" {
			t.Fatalf("got %+v, want driver 570 cuda 12.8", out)
		}
	})
	t.Run("does not clobber existing driver", func(t *testing.T) {
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{MinCUDAVersion: "12.8", MinDriverVersion: 565})
		if out.MinDriverVersion != 565 {
			t.Fatalf("MinDriverVersion = %d, want 565 (unchanged)", out.MinDriverVersion)
		}
	})
	t.Run("no cuda floor leaves req unchanged", func(t *testing.T) {
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{})
		if out.MinDriverVersion != 0 || out.MinCUDAVersion != "" {
			t.Fatalf("got %+v, want zero value", out)
		}
	})
	t.Run("unknown cuda value leaves driver zero", func(t *testing.T) {
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{MinCUDAVersion: "11.8"})
		if out.MinDriverVersion != 0 {
			t.Fatalf("MinDriverVersion = %d, want 0", out.MinDriverVersion)
		}
	})
}
