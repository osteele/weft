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

// parseCUDAImage parses an nvidia/cuda Docker image tag into its components.
// Returns ok=false for non-CUDA images.
// Expected format: "nvidia/cuda:<version>-<variant>-<os>"
// Example: "nvidia/cuda:12.4.1-devel-ubuntu22.04" → ("12.4.1", "devel", "ubuntu22.04", true)
func parseCUDAImage(image string) (version, variant, os string, ok bool) {
	if !strings.HasPrefix(image, "nvidia/cuda:") {
		return "", "", "", false
	}
	tag := strings.TrimPrefix(image, "nvidia/cuda:")

	// Split tag by '-' into version, variant, os
	// Format: <version>-<variant>-<os>
	parts := strings.SplitN(tag, "-", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}

	version = parts[0]
	variant = parts[1]
	os = parts[2]

	if _, known := cudaVariantRank[variant]; !known {
		return "", "", "", false
	}

	return version, variant, os, true
}

// imageSupremum returns the most capable compatible image for both a and b,
// or ok=false if they are incompatible.
//
// Empty string is resolved to the default cloud image before comparison.
// CUDA images with the same version and OS but different variants (base/runtime/devel)
// are compatible — the higher-capability variant is returned.
// Non-CUDA images are only compatible with exact matches.
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

	// Try CUDA image compatibility
	aVer, aVar, aOS, aOK := parseCUDAImage(a)
	bVer, bVar, bOS, bOK := parseCUDAImage(b)

	if !aOK || !bOK {
		// At least one is not a CUDA image — incompatible (already checked exact match)
		return "", false
	}

	// Same CUDA version and OS required
	if aVer != bVer || aOS != bOS {
		return "", false
	}

	// Return the higher-capability variant
	if cudaVariantRank[aVar] >= cudaVariantRank[bVar] {
		return a, true
	}
	return b, true
}
