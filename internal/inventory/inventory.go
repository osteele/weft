package inventory

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
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
	Name                string            `yaml:"name"`
	OS                  string            `yaml:"os"`
	OSRelease           string            `yaml:"os_release,omitempty"` // e.g. "ubuntu:20.04:Ubuntu 20.04.6 LTS"
	Arch                string            `yaml:"arch"`
	CPUCores            int               `yaml:"cpu_cores"`
	Memory              string            `yaml:"memory"`
	NetworkBW           string            `yaml:"network_bw"` // e.g. "1Gbps", "10Gbps"
	GPUs                []GPUSpec         `yaml:"gpus"`
	NVIDIADriverVersion string            `yaml:"nvidia_driver,omitempty"` // e.g. "550.120"
	CUDAVersion         string            `yaml:"cuda_version,omitempty"`  // max CUDA compatibility reported by the NVIDIA driver, e.g. "12.4"
	GLIBCVersion        string            `yaml:"glibc_version,omitempty"` // e.g. "2.31"
	GLIBCXXMaxVersion   string            `yaml:"glibcxx_max,omitempty"`   // max GLIBCXX symbol from libstdc++.so.6, e.g. "3.4.28"
	CPUFactor           float64           `yaml:"cpu_factor"`              // relative CPU perf (1.0 = baseline)
	GPUFactor           float64           `yaml:"gpu_factor"`              // relative GPU perf (1.0 = baseline)
	HFCacheDir          string            `yaml:"hf_cache_dir,omitempty"`  // resolved HF hub cache dir (e.g. /mnt/nas/.cache/huggingface/hub)
	SetupTimeout        string            `yaml:"setup_timeout,omitempty"` // max duration for setup commands (e.g. "90m"); default 20m
	Benchmark           BenchmarkGateSpec `yaml:"benchmark,omitempty"`
}

// BenchmarkGateSpec configures how quiet a host must be before benchmark jobs start.
type BenchmarkGateSpec struct {
	CPUThreshold  int `yaml:"cpu_threshold,omitempty"`
	RAMThreshold  int `yaml:"ram_threshold,omitempty"`
	GPUThreshold  int `yaml:"gpu_threshold,omitempty"`
	VRAMThreshold int `yaml:"vram_threshold,omitempty"`
	IdleSamples   int `yaml:"idle_samples,omitempty"`
	CheckInterval int `yaml:"check_interval,omitempty"`
}

// EnvVars returns WEFT_BENCHMARK_* assignments for configured fields.
func (b BenchmarkGateSpec) EnvVars() []string {
	var env []string
	add := func(key string, value int) {
		if value > 0 {
			env = append(env, fmt.Sprintf("%s=%d", key, value))
		}
	}
	add("WEFT_BENCHMARK_CPU", b.CPUThreshold)
	add("WEFT_BENCHMARK_RAM", b.RAMThreshold)
	add("WEFT_BENCHMARK_GPU", b.GPUThreshold)
	add("WEFT_BENCHMARK_VRAM", b.VRAMThreshold)
	add("WEFT_BENCHMARK_SAMPLES", b.IdleSamples)
	add("WEFT_BENCHMARK_INTERVAL", b.CheckInterval)
	return env
}

// BenchmarkEnvPrefix returns shell VAR=value assignments for runner startup.
func (h *HostSpec) BenchmarkEnvPrefix() string {
	if h == nil {
		return ""
	}
	var b strings.Builder
	for _, ev := range h.Benchmark.EnvVars() {
		b.WriteString(ev)
		b.WriteByte(' ')
	}
	return b.String()
}

// DefaultSetupTimeout is the default maximum duration for setup commands
// (e.g. uv sync) when no per-host override is configured.
const DefaultSetupTimeout = 20 * time.Minute

// SetupTimeoutDuration returns the configured setup timeout, or DefaultSetupTimeout
// if not set or unparseable.
func (h *HostSpec) SetupTimeoutDuration() time.Duration {
	if h.SetupTimeout == "" {
		return DefaultSetupTimeout
	}
	d, err := time.ParseDuration(h.SetupTimeout)
	if err != nil {
		return DefaultSetupTimeout
	}
	return d
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
// Bare numeric GPU names like "3090" or "2080ti" are expanded to "rtx3090", "rtx2080ti".
func NormalizeGPUClass(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	norm := b.String()
	// Expand bare RTX model numbers: "3090" -> "rtx3090", "2080ti" -> "rtx2080ti"
	if len(norm) >= 4 && norm[0] >= '0' && norm[0] <= '9' {
		suffix := strings.TrimLeft(norm, "0123456789")
		digits := norm[:len(norm)-len(suffix)]
		if len(digits) == 4 && (suffix == "" || suffix == "ti") {
			return "rtx" + norm
		}
	}
	return norm
}

// HostHFCacheDir returns the configured HF hub cache directory for a host,
// or "" if the host is not found or has no hf_cache_dir configured.
func HostHFCacheDir(name string) string {
	if spec := FindHost(name); spec != nil {
		return spec.HFCacheDir
	}
	return ""
}

// HostMaxGPUMemoryGB returns the maximum GPU memory (in GB) across all GPUs
// on a host, or 0 if the host is not found or has no GPUs.
func HostMaxGPUMemoryGB(name string) int {
	spec := FindHost(name)
	if spec == nil {
		return 0
	}
	var maxMem int
	for _, gpu := range spec.GPUs {
		if mem := ParseMemGB(gpu.Memory); mem > maxMem {
			maxMem = mem
		}
	}
	return maxMem
}

// ParseMemGB extracts an integer GiB/GB value from strings like "24GB",
// "80gb", "24576MiB", or "80".
func ParseMemGB(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0
	}

	i := 0
	for i < len(s) {
		c := s[i]
		if (c < '0' || c > '9') && c != '.' {
			break
		}
		i++
	}
	if i == 0 {
		return 0
	}
	value, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || value <= 0 {
		return 0
	}

	unit := strings.TrimSpace(s[i:])
	switch unit {
	case "", "g", "gb", "gib":
		return int(math.Floor(value))
	case "m", "mb":
		return int(math.Floor(value / 1000))
	case "mi", "mib":
		return int(math.Floor(value / 1024))
	default:
		return 0
	}
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
	if cfg.NVIDIADriverVersion != "" {
		base.NVIDIADriverVersion = cfg.NVIDIADriverVersion
	}
	if cfg.CUDAVersion != "" {
		base.CUDAVersion = cfg.CUDAVersion
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
	if cfg.HFCacheDir != "" {
		base.HFCacheDir = cfg.HFCacheDir
	}
	if cfg.Benchmark.CPUThreshold != 0 {
		base.Benchmark.CPUThreshold = cfg.Benchmark.CPUThreshold
	}
	if cfg.Benchmark.RAMThreshold != 0 {
		base.Benchmark.RAMThreshold = cfg.Benchmark.RAMThreshold
	}
	if cfg.Benchmark.GPUThreshold != 0 {
		base.Benchmark.GPUThreshold = cfg.Benchmark.GPUThreshold
	}
	if cfg.Benchmark.VRAMThreshold != 0 {
		base.Benchmark.VRAMThreshold = cfg.Benchmark.VRAMThreshold
	}
	if cfg.Benchmark.IdleSamples != 0 {
		base.Benchmark.IdleSamples = cfg.Benchmark.IdleSamples
	}
	if cfg.Benchmark.CheckInterval != 0 {
		base.Benchmark.CheckInterval = cfg.Benchmark.CheckInterval
	}
	return base
}
