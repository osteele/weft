package inventory

import (
	"embed"
	"fmt"
	"path/filepath"

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
	Name     string    `yaml:"name"`
	OS       string    `yaml:"os"`
	Arch     string    `yaml:"arch"`
	CPUCores int       `yaml:"cpu_cores"`
	Memory   string    `yaml:"memory"`
	GPUs     []GPUSpec `yaml:"gpus"`
}

// TotalGPUs returns the total number of GPU devices on the host.
func (h *HostSpec) TotalGPUs() int {
	total := 0
	for _, g := range h.GPUs {
		total += len(g.Indices)
	}
	return total
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
