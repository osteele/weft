package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

var hostsDirOverride string

// HostsDir returns the directory for host YAML files.
// Defaults to ~/.config/weft/hosts/.
func HostsDir() string {
	if hostsDirOverride != "" {
		return hostsDirOverride
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(configDir, "weft", "hosts")
}

// SetHostsDir overrides the hosts directory. Returns a cleanup function
// that restores the original value. Intended for testing.
func SetHostsDir(dir string) func() {
	old := hostsDirOverride
	hostsDirOverride = dir
	return func() { hostsDirOverride = old }
}

// LoadHostsFromDir reads all YAML host specs from a filesystem directory.
func LoadHostsFromDir(dir string) ([]HostSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read hosts dir %s: %w", dir, err)
	}

	var hosts []HostSpec
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
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

// LoadHosts loads host specs from ~/.config/weft/hosts/.
// Returns an empty slice (no error) if the directory does not exist.
func LoadHosts() ([]HostSpec, error) {
	dir := HostsDir()
	hosts, err := LoadHostsFromDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return hosts, nil
}

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

// FindHost looks up a host by name from the runtime inventory.
// Returns nil if not found.
func FindHost(name string) *HostSpec {
	hosts, err := LoadHosts()
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
