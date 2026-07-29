package cmd

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/gpucatalog"
	"github.com/osteele/weft/internal/vastai"
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

// expandGPUFlag normalizes a (gpu, gpuClass, gpuMem) triple so a non-numeric
// gpu like "nvidia>=24GB" is split into class + mem. Callers pass the raw
// inputs; receivers consume the expanded triple. Leaves numeric gpu (CUDA
// device index) and empty gpu untouched. Existing non-empty class or mem are
// not overwritten by the expansion.
func expandGPUFlag(gpu, gpuClass string, gpuMemGB int) (string, string, int, bool, error) {
	if gpu == "" || isNumericGPU(gpu) {
		return gpu, gpuClass, gpuMemGB, false, nil
	}
	parsedClass, parsedMem, err := parseGPUFlag(gpu)
	if err != nil {
		return "", "", 0, false, err
	}
	if gpuClass == "" {
		gpuClass = parsedClass
	}
	memFromGPUSelector := false
	if gpuMemGB == 0 && parsedMem > 0 {
		gpuMemGB = parsedMem
		memFromGPUSelector = true
	}
	return "", gpuClass, gpuMemGB, memFromGPUSelector, nil
}

// validateGPUSKUMemory rejects a class whose trailing memory token names a
// size the catalogue does not list for that part. A memory size inside a class
// name is an exact SKU selector, so `a100-sxm4-64gb` can never match anything
// — and once placement enforces that exactly, an unmatchable spec is
// indistinguishable at the blocker string from a market that happens to be
// empty. Fail at submit, where the user can still fix the typo.
//
// Uncatalogued classes pass. Weft cannot validate a part it has never heard
// of, and refusing one would make a stale catalogue look like a bad request.
//
// Deliberately not called from expandGPUFlag: restart applies that helper to
// script metadata and CLI overrides separately, before precedence merges them,
// so validating there would reject a bad script value the CLI legitimately
// overrides. Validate the resolved class instead.
func validateGPUSKUMemory(gpuClass string) error {
	base, requestedGB := gpucatalog.SplitTrailingMemorySuffix(gpuClass)
	if requestedGB == 0 {
		return nil
	}
	sizes, known := vastai.HardwareMemorySizesGB(base)
	if !known {
		return nil
	}
	if slices.Contains(sizes, requestedGB) {
		return nil
	}
	// Order by capacity, not lexically: a string sort renders {8, 16} as
	// "16GB, 8GB".
	slices.Sort(sizes)
	labels := make([]string, len(sizes))
	for i, size := range sizes {
		labels[i] = fmt.Sprintf("%dGB", size)
	}
	return fmt.Errorf(
		"--gpu %s: %s does not ship in %dGB (known sizes: %s).\n"+
			"A memory size inside a GPU class names an exact part. For a floor instead, use --gpu %q",
		gpuClass, base, requestedGB, strings.Join(labels, ", "),
		fmt.Sprintf("%s>=%dGB", base, requestedGB))
}
