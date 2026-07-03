package campaign

import (
	"strings"

	"github.com/osteele/weft/internal/inventory"
)

func broadNVIDIAConstraint(requested string) bool {
	return inventory.NormalizeGPUClass(requested) == "nvidia"
}

func premiumAcceleratorClass(instanceClass, resolvedGPUName string) bool {
	for _, candidate := range []string{instanceClass, resolvedGPUName} {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			continue
		}
		normalized := inventory.NormalizeGPUClass(candidate)
		for _, needle := range []string{"h100", "h200", "b100", "b200", "gh200", "gb200"} {
			if strings.Contains(candidate, needle) || strings.Contains(normalized, needle) {
				return true
			}
		}
	}
	return false
}
