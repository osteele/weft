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
		{"12.9", 575},
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
	t.Run("preserves driver that already satisfies CUDA floor", func(t *testing.T) {
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{MinCUDAVersion: "12.8", MinDriverVersion: 580})
		if out.MinDriverVersion != 580 {
			t.Fatalf("MinDriverVersion = %d, want 580 (preserved, ≥ CUDA-implied 570)", out.MinDriverVersion)
		}
	})
	t.Run("raises driver that does not satisfy CUDA floor", func(t *testing.T) {
		// Regression: a previously back-filled or stale driver below the
		// current CUDA floor must be raised, otherwise an image-label
		// merge that bumps MinCUDAVersion leaves the driver stuck at the
		// old value and Vast returns offers that fail at runtime with
		// cuda_driver_too_old.
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{MinCUDAVersion: "12.8", MinDriverVersion: 550})
		if out.MinDriverVersion != 570 {
			t.Fatalf("MinDriverVersion = %d, want 570 (raised from stale 550 to satisfy CUDA 12.8)", out.MinDriverVersion)
		}
	})
	t.Run("no cuda floor leaves req unchanged", func(t *testing.T) {
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{})
		if out.MinDriverVersion != 0 || out.MinCUDAVersion != "" {
			t.Fatalf("got %+v, want zero value", out)
		}
	})
	t.Run("cuda below lowest tabled leaves driver zero", func(t *testing.T) {
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{MinCUDAVersion: "11.8"})
		if out.MinDriverVersion != 0 {
			t.Fatalf("MinDriverVersion = %d, want 0", out.MinDriverVersion)
		}
	})
	t.Run("cuda above highest tabled refuses to guess driver", func(t *testing.T) {
		// Future CUDA toolkits must not silently get the highest-known
		// driver — that would under-constrain placement. Caller should
		// see 0 and surface the unknown.
		out := BackfillDriverFromCUDA(cloud.ImageRequirements{MinCUDAVersion: "14.0"})
		if out.MinDriverVersion != 0 {
			t.Fatalf("MinDriverVersion = %d, want 0 (refuse to guess for future CUDA 14.0)", out.MinDriverVersion)
		}
	})
}

func TestMinDriverForCUDA_TwoDigitMinor(t *testing.T) {
	// Component-wise compare: a hypothetical CUDA 12.10 must sort AFTER
	// 12.8 and 12.9, not before (the old float-parse path treated "12.10" as
	// 12.1). With the 12.9 row present, 12.10 takes the 12.9 floor.
	if got := MinDriverForCUDA("12.10"); got != 575 {
		t.Errorf("MinDriverForCUDA(\"12.10\") = %d, want 575 (between 12.9 and 13.0, takes 12.9 floor)", got)
	}
}
