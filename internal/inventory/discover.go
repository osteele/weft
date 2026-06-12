package inventory

import (
	"log/slog"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/hostinfo"
)

// DetectHFCacheDirCommand returns a shell command that prints the resolved HF
// hub cache directory on a remote host, honouring HF_HUB_CACHE > HF_HOME > default.
func DetectHFCacheDirCommand() string {
	return dataloc.ResolveHFCacheDirShellVar() + `; echo "$_hf_cache"`
}

// HostSpecFromHostInfo converts probed runtime host information to a static HostSpec.
// hfCacheDir is the resolved HF hub cache directory detected on the host; pass ""
// if detection was not performed or failed.
func HostSpecFromHostInfo(name string, info *hostinfo.Host, hfCacheDir string) HostSpec {
	cpuFactor, cpuKnown := LookupCPUFactor(info.CPUModel)
	if !cpuKnown && info.CPUModel != "" {
		slog.Warn("unknown CPU model, using cpu_factor=1.0", "component", "inventory", "cpu_model", info.CPUModel)
	}

	spec := HostSpec{
		Name:                name,
		CPUCores:            info.CPUs,
		Memory:              info.MemTotal,
		NVIDIADriverVersion: strings.TrimSpace(info.NVIDIADriverVersion),
		CUDAVersion:         strings.TrimSpace(info.CUDAVersion),
		OSRelease:           strings.TrimSpace(info.OSRelease),
		GLIBCVersion:        strings.TrimSpace(info.GLIBCVersion),
		GLIBCXXMaxVersion:   strings.TrimSpace(info.GLIBCXXMaxVersion),
		CPUFactor:           cpuFactor,
		GPUFactor:           1.0,
		HFCacheDir:          strings.TrimSpace(hfCacheDir),
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
		g.Name = resolveTruncatedGPUName(g.Name, g.MemTotal)
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

func resolveTruncatedGPUName(name, mem string) string {
	normalized := NormalizeGPUClass(name)
	if !strings.HasPrefix(normalized, "nvidiageforce") {
		return name
	}
	if !strings.Contains(name, "...") && normalized != "nvidiageforce" {
		return name
	}
	switch ParseMemGB(mem) {
	case 24:
		return "NVIDIA GeForce RTX 3090"
	case 11:
		return "NVIDIA GeForce RTX 2080 Ti"
	default:
		return name
	}
}
