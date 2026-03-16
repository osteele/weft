package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/osteele/weft/internal/config"
)

var hostsOverride []HostSpec
var hostsDirOverride string

// HostsDir returns the directory for discovered host YAML files.
// Defaults to ~/.config/weft/hosts/ (same base as config.toml).
func HostsDir() string {
	if hostsDirOverride != "" {
		return hostsDirOverride
	}
	home := os.Getenv("HOME")
	return filepath.Join(home, ".config", "weft", "hosts")
}

// SetHosts overrides the runtime inventory. Intended for testing.
func SetHosts(hosts []HostSpec) func() {
	old := hostsOverride
	hostsOverride = slices.Clone(hosts)
	return func() { hostsOverride = old }
}

// SetHostsDir overrides the discovered-hosts directory. Intended for testing.
func SetHostsDir(dir string) func() {
	old := hostsDirOverride
	hostsDirOverride = dir
	return func() { hostsDirOverride = old }
}

// LoadHostsFromDir reads all YAML host specs from a filesystem directory.
// Retained for tests and migration helpers.
func LoadHostsFromDir(dir string) ([]HostSpec, error) {
	return loadHostsFromDir(dir)
}

// LoadHosts loads host specs from discovered host files and [hosts.<name>] in
// ~/.config/weft/config.toml. Config entries override discovered host specs
// when the same host appears in both places.
func LoadHosts() ([]HostSpec, error) {
	if hostsOverride != nil {
		return slices.Clone(hostsOverride), nil
	}

	merged := make(map[string]HostSpec)
	discovered, err := loadHostsFromDir(HostsDir())
	if err == nil {
		for _, host := range discovered {
			merged[host.Name] = host
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(merged)+len(cfg.Hosts))
	for name := range merged {
		names = append(names, name)
	}
	for name, hostCfg := range cfg.Hosts {
		base := merged[name] // zero value if not in discovered hosts
		base.Name = name
		merged[name] = applyHostConfig(base, hostCfg)
	}
	names = names[:0]
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	hosts := make([]HostSpec, 0, len(names))
	for _, name := range names {
		hosts = append(hosts, merged[name])
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

// applyHostConfig merges non-zero fields from cfg into base, returning the result.
// This lets config.toml entries augment or override YAML-discovered specs without
// wiping out fields that config.toml doesn't mention.
func applyHostConfig(base HostSpec, cfg config.HostConfig) HostSpec {
	if cfg.OS != "" {
		base.OS = cfg.OS
	}
	if cfg.Arch != "" {
		base.Arch = cfg.Arch
	}
	if cfg.CPUCores != 0 {
		base.CPUCores = cfg.CPUCores
	}
	if cfg.Memory != "" {
		base.Memory = cfg.Memory
	}
	if cfg.NetworkBW != "" {
		base.NetworkBW = cfg.NetworkBW
	}
	if len(cfg.GPUs) > 0 {
		gpus := make([]GPUSpec, 0, len(cfg.GPUs))
		for _, gpu := range cfg.GPUs {
			gpus = append(gpus, GPUSpec{
				Name:    gpu.Name,
				Class:   gpu.Class,
				Memory:  gpu.Memory,
				Indices: slices.Clone(gpu.Indices),
			})
		}
		base.GPUs = gpus
	}
	if cfg.CPUFactor != 0 {
		base.CPUFactor = cfg.CPUFactor
	}
	if cfg.GPUFactor != 0 {
		base.GPUFactor = cfg.GPUFactor
	}
	return base
}
