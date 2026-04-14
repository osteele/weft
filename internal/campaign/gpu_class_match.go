package campaign

import (
	"strings"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/vastai"
)

func gpuClassCompatible(requested, instanceClass, resolvedGPUName string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return true
	}

	for _, candidate := range []string{instanceClass, resolvedGPUName} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.EqualFold(requested, candidate) {
			return true
		}
		if normalizedGPUClassEquivalent(requested, candidate) {
			return true
		}
	}

	if resolved := strings.TrimSpace(resolvedGPUName); resolved != "" {
		if vastai.GPUClassMatchesOfferName(requested, resolved) {
			return true
		}
	}
	if instClass := strings.TrimSpace(instanceClass); instClass != "" {
		if vastai.GPUClassMatchesOfferName(requested, instClass) {
			return true
		}
	}
	return false
}

func normalizedGPUClassEquivalent(a, b string) bool {
	left := normalizedGPUClassVariants(inventory.NormalizeGPUClass(a))
	right := normalizedGPUClassVariants(inventory.NormalizeGPUClass(b))
	for _, l := range left {
		for _, r := range right {
			if l == r {
				return true
			}
		}
	}
	return false
}

func normalizedGPUClassVariants(norm string) []string {
	if norm == "" {
		return nil
	}
	var variants []string
	add := func(v string) {
		if v == "" {
			return
		}
		for _, existing := range variants {
			if existing == v {
				return
			}
		}
		variants = append(variants, v)
	}
	add(norm)
	if strings.HasPrefix(norm, "rtx") {
		add(strings.TrimPrefix(norm, "rtx"))
	} else {
		add("rtx" + norm)
	}
	return variants
}
