package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// UseTestHosts writes test host YAML files to a temp directory and sets the
// hosts dir override so that LoadHosts() returns these fixtures. Call from
// TestMain or individual tests. The cleanup is registered via t.Cleanup.
func UseTestHosts(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, h := range TestHosts() {
		data, err := yaml.Marshal(h)
		if err != nil {
			t.Fatalf("marshal test host %s: %v", h.Name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, h.Name+".yaml"), data, 0644); err != nil {
			t.Fatalf("write test host %s: %v", h.Name, err)
		}
	}
	cleanup := SetHostsDir(dir)
	t.Cleanup(cleanup)
}

// TestHosts returns fictional host specs for use in tests.
// These mirror realistic hardware configurations without referencing
// any site-specific machines.
func TestHosts() []HostSpec {
	return []HostSpec{
		{
			Name:      "host-alpha",
			OS:        "linux",
			Arch:      "amd64",
			CPUCores:  64,
			Memory:    "256GB",
			NetworkBW: "10Gbps",
			CPUFactor: 1.0,
			GPUFactor: 1.0,
			GPUs: []GPUSpec{
				{Name: "A100 80GB PCIe", Class: "a100", Memory: "80GB", Indices: []int{0, 1}},
				{Name: "RTX 2080 Ti", Class: "rtx2080ti", Memory: "11GB", Indices: []int{2, 3, 4, 5, 6, 7, 8, 9}},
			},
		},
		{
			Name:      "host-beta",
			OS:        "linux",
			Arch:      "amd64",
			CPUCores:  16,
			Memory:    "64GB",
			NetworkBW: "1Gbps",
			CPUFactor: 0.5,
			GPUFactor: 0.6,
			GPUs: []GPUSpec{
				{Name: "RTX 3090", Class: "rtx3090", Memory: "24GB", Indices: []int{0}},
			},
		},
		{
			Name:      "host-gamma",
			OS:        "darwin",
			Arch:      "arm64",
			CPUCores:  12,
			Memory:    "96GB",
			NetworkBW: "1Gbps",
			CPUFactor: 2.0,
			GPUFactor: 0.15,
			GPUs: []GPUSpec{
				{Name: "M2 Max", Class: "m2max", Memory: "96GB", Indices: []int{0}},
			},
		},
	}
}

// FindTestHost returns a test host by name, or nil if not found.
func FindTestHost(name string) *HostSpec {
	for _, h := range TestHosts() {
		if h.Name == name {
			return &h
		}
	}
	return nil
}
