package campaign

import "testing"

func TestParseCUDAImage(t *testing.T) {
	tests := []struct {
		image                string
		version, variant, os string
		ok                   bool
	}{
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "12.4.1", "runtime", "ubuntu22.04", true},
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "12.4.1", "devel", "ubuntu22.04", true},
		{"nvidia/cuda:12.4.1-base-ubuntu22.04", "12.4.1", "base", "ubuntu22.04", true},
		{"nvidia/cuda:12.6.0-runtime-ubuntu24.04", "12.6.0", "runtime", "ubuntu24.04", true},

		// Non-CUDA images
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "", "", "", false},
		{"ubuntu:22.04", "", "", "", false},
		{"", "", "", "", false},

		// Malformed CUDA tags
		{"nvidia/cuda:12.4.1", "", "", "", false},
		{"nvidia/cuda:12.4.1-unknown-ubuntu22.04", "", "", "", false},
	}

	for _, tt := range tests {
		version, variant, os, ok := parseCUDAImage(tt.image)
		if ok != tt.ok {
			t.Errorf("parseCUDAImage(%q) ok = %v, want %v", tt.image, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if version != tt.version || variant != tt.variant || os != tt.os {
			t.Errorf("parseCUDAImage(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tt.image, version, variant, os, tt.version, tt.variant, tt.os)
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

		// devel > runtime (same version+OS)
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},

		// devel > base
		{"nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-base-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},

		// runtime > base
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-base-ubuntu22.04", "nvidia/cuda:12.4.1-runtime-ubuntu22.04", true},

		// Empty (default=runtime) + devel → devel
		{"", "nvidia/cuda:12.4.1-devel-ubuntu22.04", "nvidia/cuda:12.4.1-devel-ubuntu22.04", true},

		// Different CUDA version — incompatible
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.6.0-runtime-ubuntu22.04", "", false},

		// Different OS — incompatible
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "nvidia/cuda:12.4.1-runtime-ubuntu24.04", "", false},

		// Non-CUDA image — incompatible with CUDA
		{"nvidia/cuda:12.4.1-runtime-ubuntu22.04", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "", false},

		// Non-CUDA images — only exact match
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", true},
		{"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", "pytorch/pytorch:2.5.0-cuda12.4-cudnn9-runtime", "", false},
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
