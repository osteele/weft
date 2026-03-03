package cmd

import (
	"fmt"
	"strconv"
	"strings"
)

// parseGPUFlag parses a combined GPU flag value like "nvidia>=24GB" into
// a GPU class spec and optional memory requirement in GB.
// If no ">=" separator is present, returns (value, 0, nil).
func parseGPUFlag(value string) (gpuClass string, gpuMemGB int, err error) {
	parts := strings.SplitN(value, ">=", 2)
	if len(parts) == 1 {
		return value, 0, nil
	}

	gpuClass = parts[0]
	if gpuClass == "" {
		return "", 0, fmt.Errorf("missing GPU class before '>='")
	}

	memStr := strings.TrimSpace(parts[1])
	if memStr == "" {
		return "", 0, fmt.Errorf("missing memory value after '>='")
	}

	// Strip optional "GB" suffix (case insensitive)
	memStr = strings.TrimSuffix(strings.TrimSuffix(memStr, "GB"), "gb")

	mem, err := strconv.Atoi(memStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid memory value %q: must be a number (optionally followed by GB)", parts[1])
	}
	if mem <= 0 {
		return "", 0, fmt.Errorf("memory value must be positive, got %d", mem)
	}

	return gpuClass, mem, nil
}
