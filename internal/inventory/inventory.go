package inventory

import (
	"embed"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

//go:embed hosts/*.yaml
var hostsFS embed.FS

// GPUSpec describes a group of identical GPUs on a host.
type GPUSpec struct {
	Name    string `yaml:"name"`
	Class   string `yaml:"class"`
	Memory  string `yaml:"memory"`
	Indices []int  `yaml:"indices"`
}

// HostSpec describes the static capabilities of a host.
type HostSpec struct {
	Name      string    `yaml:"name"`
	OS        string    `yaml:"os"`
	Arch      string    `yaml:"arch"`
	CPUCores  int       `yaml:"cpu_cores"`
	Memory    string    `yaml:"memory"`
	NetworkBW string    `yaml:"network_bw"` // e.g. "1Gbps", "10Gbps"
	GPUs      []GPUSpec `yaml:"gpus"`
	CPUFactor float64   `yaml:"cpu_factor"` // relative CPU perf (1.0 = baseline)
	GPUFactor float64   `yaml:"gpu_factor"` // relative GPU perf (1.0 = baseline)
}

// CPUPerformance returns the CPU performance factor, defaulting to 1.0 if unset.
func (h *HostSpec) CPUPerformance() float64 {
	if h.CPUFactor == 0 {
		return 1.0
	}
	return h.CPUFactor
}

// GPUPerformance returns the GPU performance factor, defaulting to 1.0 if unset.
func (h *HostSpec) GPUPerformance() float64 {
	if h.GPUFactor == 0 {
		return 1.0
	}
	return h.GPUFactor
}

// NetworkBWBytesPerSec returns the network bandwidth in bytes per second.
// Returns 0 if the bandwidth is not set or cannot be parsed.
func (h *HostSpec) NetworkBWBytesPerSec() float64 {
	return ParseNetworkBW(h.NetworkBW)
}

// ParseNetworkBW parses a bandwidth string like "1Gbps" or "10Gbps" and
// returns bytes per second. Returns 0 for unparseable values.
func ParseNetworkBW(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, "bps")

	var value float64
	var unit string
	n, _ := fmt.Sscanf(s, "%f%s", &value, &unit)
	if n == 0 {
		return 0
	}
	if n == 1 {
		// bare number, assume bits per second
		return value / 8
	}

	switch unit {
	case "g":
		return value * 1e9 / 8
	case "m":
		return value * 1e6 / 8
	case "k":
		return value * 1e3 / 8
	default:
		return value / 8
	}
}

// TotalGPUs returns the total number of GPU devices on the host.
func (h *HostSpec) TotalGPUs() int {
	total := 0
	for _, g := range h.GPUs {
		total += len(g.Indices)
	}
	return total
}

// NormalizeGPUClass strips spaces, punctuation, and lowercases for fuzzy matching.
// e.g. "RTX 3090", "rtx3090", "rtx-3090" all normalize to "rtx3090".
func NormalizeGPUClass(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LoadEmbeddedHosts parses all embedded host YAML files and returns the specs.
func LoadEmbeddedHosts() ([]HostSpec, error) {
	entries, err := hostsFS.ReadDir("hosts")
	if err != nil {
		return nil, fmt.Errorf("read embedded hosts dir: %w", err)
	}

	var hosts []HostSpec
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := hostsFS.ReadFile("hosts/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		var spec HostSpec
		if err := yaml.Unmarshal(data, &spec); err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		hosts = append(hosts, spec)
	}
	return hosts, nil
}

// FindHost looks up a host by name from the embedded inventory.
// Returns nil if not found.
func FindHost(name string) *HostSpec {
	hosts, err := LoadEmbeddedHosts()
	if err != nil {
		return nil
	}
	for i := range hosts {
		if hosts[i].Name == name {
			return &hosts[i]
		}
	}
	return nil
}
