package inventory

import (
	"strings"

	"github.com/osteele/weft/internal/hostinfo"
)

// HostSpecFromHostInfo converts probed runtime host information to a static HostSpec.
func HostSpecFromHostInfo(name string, info *hostinfo.Host) HostSpec {
	spec := HostSpec{
		Name:      name,
		CPUCores:  info.CPUs,
		Memory:    info.MemTotal,
		CPUFactor: 1.0,
		GPUFactor: 1.0,
	}

	// Parse "Linux x86_64" or "Darwin arm64"
	spec.OS, spec.Arch = parseArch(info.Arch)

	// Group identical GPUs
	spec.GPUs = groupGPUs(info.GPUs)

	return spec
}

// parseArch converts "Linux x86_64" → ("linux", "amd64"), "Darwin arm64" → ("darwin", "arm64").
func parseArch(arch string) (os, goarch string) {
	parts := strings.Fields(arch)
	if len(parts) >= 1 {
		os = strings.ToLower(parts[0])
	}
	if len(parts) >= 2 {
		goarch = normalizeArch(parts[1])
	}
	return
}

// normalizeArch maps uname arch strings to Go arch names.
func normalizeArch(arch string) string {
	switch strings.ToLower(arch) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(arch)
	}
}

// groupGPUs groups identical GPUs by name and derives class/memory.
func groupGPUs(gpus []hostinfo.GPUInfo) []GPUSpec {
	var order []string
	groups := map[string]*GPUSpec{}

	for _, g := range gpus {
		if spec, ok := groups[g.Name]; ok {
			spec.Indices = append(spec.Indices, g.Index)
		} else {
			groups[g.Name] = &GPUSpec{
				Name:    g.Name,
				Class:   NormalizeGPUClass(g.Name),
				Memory:  g.MemTotal,
				Indices: []int{g.Index},
			}
			order = append(order, g.Name)
		}
	}

	result := make([]GPUSpec, 0, len(order))
	for _, name := range order {
		result = append(result, *groups[name])
	}
	return result
}
