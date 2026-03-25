package campaign

import (
	"strings"
	"testing"
)

func TestTorchImageForCUDAVersion(t *testing.T) {
	// Known CUDA version returns a pytorch image
	img := torchImageForCUDAVersion("12.4")
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

	// Unknown CUDA version returns empty
	if got := torchImageForCUDAVersion("11.8"); got != "" {
		t.Errorf("expected empty for unknown CUDA version, got %q", got)
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
