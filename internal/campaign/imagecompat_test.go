package campaign

import (
	"strings"
	"testing"
)

func TestTorchImageForCUDAVersion(t *testing.T) {
	// Known CUDA version returns a pytorch image
	img := torchImageForCUDAVersion("12.4", false)
	if img == "" {
		t.Fatal("expected pytorch image for CUDA 12.4")
	}
	if !strings.HasPrefix(img, "pytorch/pytorch:") {
		t.Errorf("expected pytorch/pytorch: prefix, got %q", img)
	}
	// Verify the image parses correctly
	cudaVer, _, ok := parseCUDAImage(img)
	if !ok {
		t.Fatalf("torchImageForCUDAVersion returned unparseable image %q", img)
	}
	if cudaVer != "12.4" {
		t.Errorf("image CUDA version = %q, want 12.4", cudaVer)
	}

	// devel variant
	if got := torchImageForCUDAVersion("12.4", true); !strings.Contains(got, "-devel") {
		t.Errorf("expected devel variant, got %q", got)
	}

	// Unknown CUDA version returns empty
	if got := torchImageForCUDAVersion("11.8", false); got != "" {
		t.Errorf("expected empty for unknown CUDA version, got %q", got)
	}

	if got := torchImageForCUDAVersion("12.8", false); got == "" {
		t.Error("expected pytorch image for CUDA 12.8")
	}

	// Unmapped same-major version degrades to the next mapped version within
	// the major (e.g., 12.7 -> 12.8), not falling back to 12.4.
	if got := torchImageForCUDAVersion("12.7", false); got != "pytorch/pytorch:2.7.0-cuda12.8-cudnn9-runtime" {
		t.Errorf("torchImageForCUDAVersion(12.7) = %q, want 12.8 image", got)
	}

	// Cross-major lookup is refused.
	if got := torchImageForCUDAVersion("13.5", false); got != "" {
		t.Errorf("expected empty for cross-major CUDA version, got %q", got)
	}
}

func TestDefaultCUDAImageForVersion(t *testing.T) {
	if got := defaultCUDAImageForVersion("12.4", false); got != "nvidia/cuda:12.4.1-runtime-ubuntu22.04" {
		t.Errorf("defaultCUDAImageForVersion(12.4,false) = %q", got)
	}
	if got := defaultCUDAImageForVersion("12.4", true); got != "nvidia/cuda:12.4.1-devel-ubuntu22.04" {
		t.Errorf("defaultCUDAImageForVersion(12.4,true) = %q", got)
	}
	if got := defaultCUDAImageForVersion("12.7", false); got != "nvidia/cuda:12.8.1-runtime-ubuntu22.04" {
		t.Errorf("defaultCUDAImageForVersion(12.7,false) = %q, want 12.8 image", got)
	}
	if got := defaultCUDAImageForVersion("11.8", false); got != "" {
		t.Errorf("expected empty for unknown CUDA version, got %q", got)
	}
}

func TestChooseAutoImageForMinCUDA(t *testing.T) {
	if got := chooseAutoImageForMinCUDA(12.8, false, false); got != "nvidia/cuda:12.8.1-runtime-ubuntu22.04" {
		t.Fatalf("chooseAutoImageForMinCUDA(12.8,false,false) = %q", got)
	}
	if got := chooseAutoImageForMinCUDA(12.8, true, false); got != "pytorch/pytorch:2.7.0-cuda12.8-cudnn9-runtime" {
		t.Fatalf("chooseAutoImageForMinCUDA(12.8,true,false) = %q", got)
	}
	if got := chooseAutoImageForMinCUDA(12.8, false, true); got != "nvidia/cuda:12.8.1-devel-ubuntu22.04" {
		t.Fatalf("chooseAutoImageForMinCUDA(12.8,false,true) = %q", got)
	}
}

func TestValidateCUDADriverMinOverrideAllowsEmptyValue(t *testing.T) {
	if err := ValidateCUDADriverMinOverride(""); err != nil {
		t.Fatalf("empty CUDA driver floor should be accepted: %v", err)
	}
}

func TestValidateCUDADriverMinOverrideAllowsAny(t *testing.T) {
	if err := ValidateCUDADriverMinOverride("any"); err != nil {
		t.Fatalf("cuda-driver-min=any should be accepted: %v", err)
	}
}

func TestValidateCUDADriverMinOverrideAllowsVersion(t *testing.T) {
	if err := ValidateCUDADriverMinOverride("12.5"); err != nil {
		t.Fatalf("valid CUDA driver floor should be accepted: %v", err)
	}
}

func TestValidateCUDADriverMinOverrideRejectsInvalidValue(t *testing.T) {
	err := ValidateCUDADriverMinOverride("not-a-cuda-version")
	if err == nil {
		t.Fatal("expected invalid cuda-driver-min to be rejected")
	}
	if !strings.Contains(err.Error(), "unrecognized CUDA driver floor") {
		t.Fatalf("error %q missing invalid floor context", err)
	}
}

func TestParseCUDAImage(t *testing.T) {
	tests := []struct {
		image                   string
		cudaMajorMinor, variant string
		ok                      bool
	}{
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "12.4", "runtime", true},
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "12.4", "devel", true},
		{"nvidia/cuda:12.4.1-base-ubuntu22.04", "12.4", "base", true},
		{"nvidia/cuda:12.6.0-runtime-ubuntu24.04", "12.6", "runtime", true},

		// PyTorch images
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "12.4", "runtime", true},
		{"pytorch/pytorch:2.5.0-cuda12.4-cudnn9-devel", "12.4", "devel", true},

		// Non-CUDA images
		{"ubuntu:22.04", "", "", false},
		{"", "", "", false},

		// Malformed CUDA tags
		{"nvidia/cuda:12.4.1", "", "", false},
		{"nvidia/cuda:12.4.1-unknown-ubuntu22.04", "", "", false},
	}

	for _, tt := range tests {
		cudaMajorMinor, variant, ok := parseCUDAImage(tt.image)
		if ok != tt.ok {
			t.Errorf("parseCUDAImage(%q) ok = %v, want %v", tt.image, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if cudaMajorMinor != tt.cudaMajorMinor || variant != tt.variant {
			t.Errorf("parseCUDAImage(%q) = (%q, %q), want (%q, %q)",
				tt.image, cudaMajorMinor, variant, tt.cudaMajorMinor, tt.variant)
		}
	}
}

func TestImageSupremum(t *testing.T) {
	tests := []struct {
		a, b       string
		wantMerged string
		wantOK     bool
	}{
		// Empty resolves to default (runtime)
		{"", "", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", true},
		{"", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", true},
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", true},

		// Same image
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},

		// devel > runtime (same CUDA major.minor)
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},

		// devel > base
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-base-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},

		// runtime > base
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-base-ubuntu22.04", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", true},

		// Empty (default=runtime) + devel → devel
		{"", "nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},

		// Different CUDA major.minor — incompatible
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.6.0-runtime-ubuntu22.04", "", false},

		// pytorch/pytorch subsumes nvidia/cuda runtime (same CUDA major.minor)
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", true},
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", true},

		// Empty (default=12.4 runtime) + pytorch 12.4 runtime → pytorch
		{"", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", true},

		// pytorch with different CUDA version — incompatible
		{"nvidia/cuda:12.6.0-runtime-ubuntu22.04", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "", false},

		// Non-CUDA images — only exact match
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", true},
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.5.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", true},

		// Cross-framework with different variant ranks — incompatible
		// pytorch runtime doesn't have build tools; nvidia devel doesn't have pytorch
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "nvidia/cuda:12.4.1-devel-ubuntu22.04", "", false},
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "", false},

		// Cross-framework same variant — pytorch wins
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-devel", "nvidia/cuda:12.4.1-devel-ubuntu22.04", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-devel", true},

		// Non-CUDA image — incompatible
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "ubuntu:22.04", "", false},

		// Legacy private SGLang runtime aliases normalize to the public image.
		{"ghcr.io/osteele/sglang-runtime:v0.5.10.post1", sglangRuntimeImage, sglangRuntimeImage, true},
		{"sglang:dev-cu13", sglangDevCU13RuntimeImage, sglangDevCU13RuntimeImage, true},
	}

	for _, tt := range tests {
		merged, ok := imageSupremum(tt.a, tt.b)
		if ok != tt.wantOK {
			t.Errorf("imageSupremum(%q, %q) ok = %v, want %v", tt.a, tt.b, ok, tt.wantOK)
			continue
		}
		if ok && merged != tt.wantMerged {
			t.Errorf("imageSupremum(%q, %q) = %q, want %q", tt.a, tt.b, merged, tt.wantMerged)
		}
	}
}
