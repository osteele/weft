package campaign

import (
	"strings"

	"github.com/osteele/weft/internal/cloud"
)

// cudaVariantRank orders NVIDIA CUDA image variants by capability.
// Higher rank means more capabilities (devel includes runtime which includes base).
var cudaVariantRank = map[string]int{
	"base":    0,
	"runtime": 1,
	"devel":   2,
}

// imageFrameworkRank orders images by framework richness at the same variant level.
// pytorch/pytorch includes CUDA runtime + PyTorch, so it subsumes nvidia/cuda runtime.
var imageFrameworkRank = map[string]int{
	"nvidia/cuda":     0,
	"pytorch/pytorch": 1,
}

// parseCUDAImage parses a CUDA-based Docker image tag into its components.
// Supports nvidia/cuda and pytorch/pytorch image formats.
// Returns ok=false for non-CUDA images.
//
// nvidia/cuda format: "nvidia/cuda:<version>-<variant>-<os>"
// Example: "nvidia/cuda:12.4.1-devel-ubuntu22.04" → ("12.4", "devel", true)
//
// pytorch/pytorch format: "pytorch/pytorch:<ver>-cuda<cuda_ver>-cudnn<N>-<variant>"
// Example: "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime" → ("12.4", "runtime", true)
// pytorch images subsume nvidia/cuda runtime images (they include CUDA runtime + PyTorch).
func parseCUDAImage(image string) (cudaMajorMinor, variant string, ok bool) {
	switch {
	case strings.HasPrefix(image, "nvidia/cuda:"):
		tag := strings.TrimPrefix(image, "nvidia/cuda:")
		parts := strings.SplitN(tag, "-", 3)
		if len(parts) != 3 {
			return "", "", false
		}
		variant = parts[1]
		if _, known := cudaVariantRank[variant]; !known {
			return "", "", false
		}
		cudaMajorMinor = cudaMajorMinorVersion(parts[0])
		return cudaMajorMinor, variant, true

	case strings.HasPrefix(image, "pytorch/pytorch:"):
		tag := strings.TrimPrefix(image, "pytorch/pytorch:")
		// Format: <pytorch_ver>-cuda<cuda_ver>-cudnn<N>-<variant>
		parts := strings.Split(tag, "-")
		if len(parts) < 4 {
			return "", "", false
		}
		// Find the cuda<ver> part
		for _, p := range parts {
			if strings.HasPrefix(p, "cuda") {
				cudaVer := strings.TrimPrefix(p, "cuda")
				variant = parts[len(parts)-1]
				if _, known := cudaVariantRank[variant]; !known {
					return "", "", false
				}
				return cudaMajorMinorVersion(cudaVer), variant, true
			}
		}
		return "", "", false

	default:
		return "", "", false
	}
}

// imageRepo extracts the repository prefix from a Docker image (e.g., "nvidia/cuda" from "nvidia/cuda:12.4.1-...").
func imageRepo(image string) string {
	if idx := strings.Index(image, ":"); idx >= 0 {
		return image[:idx]
	}
	return image
}

// cudaMajorMinorVersion extracts "12.4" from "12.4.1" or returns "12.4" unchanged.
func cudaMajorMinorVersion(ver string) string {
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return ver
}

// isPyTorchImage returns true if the image is a pytorch/pytorch image.
func isPyTorchImage(image string) bool {
	return imageRepo(image) == "pytorch/pytorch"
}

// torchImageForCUDAVersion returns a popular PyTorch Docker image tag for the
// given CUDA major.minor version, or "" if no known image exists. The selected
// images are widely used on Vast.ai and likely pre-cached in data centers.
func torchImageForCUDAVersion(cudaMajorMinor string) string {
	switch cudaMajorMinor {
	case "12.4":
		return "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime"
	default:
		return ""
	}
}

// imageSupremum returns the most capable compatible image for both a and b,
// or ok=false if they are incompatible.
//
// Empty string is resolved to the default cloud image before comparison.
// CUDA images with the same version and OS but different variants (base/runtime/devel)
// are compatible — the higher-capability variant is returned.
// Non-CUDA images are only compatible with exact matches.
//
// When comparing across frameworks (pytorch vs nvidia/cuda), pytorch subsumes
// nvidia/cuda at runtime level, so pytorch/pytorch:...-runtime beats
// nvidia/cuda:...-runtime. However, nvidia/cuda:...-devel (with nvcc/build tools)
// is incompatible with pytorch/pytorch:...-runtime since neither fully subsumes
// the other.
func imageSupremum(a, b string) (merged string, ok bool) {
	// Resolve empty to default
	if a == "" {
		a = cloud.DefaultImage
	}
	if b == "" {
		b = cloud.DefaultImage
	}

	// Exact match
	if a == b {
		return a, true
	}

	// Try CUDA image compatibility (supports nvidia/cuda and pytorch/pytorch)
	aVer, aVar, aOK := parseCUDAImage(a)
	bVer, bVar, bOK := parseCUDAImage(b)

	if !aOK || !bOK {
		return "", false
	}

	// Same CUDA major.minor version required
	if aVer != bVer {
		return "", false
	}

	aFramework := imageFrameworkRank[imageRepo(a)]
	bFramework := imageFrameworkRank[imageRepo(b)]
	aRank := cudaVariantRank[aVar]
	bRank := cudaVariantRank[bVar]

	// Cross-framework with different variant ranks: incompatible.
	// e.g. pytorch/pytorch:...-runtime vs nvidia/cuda:...-devel —
	// neither fully subsumes the other.
	if aFramework != bFramework && aRank != bRank {
		return "", false
	}

	// Same framework: pick higher variant rank
	if aFramework == bFramework {
		if aRank >= bRank {
			return a, true
		}
		return b, true
	}

	// PyTorch images are supersets of plain CUDA images at the same variant level.
	if aFramework >= bFramework {
		return a, true
	}
	return b, true
}
