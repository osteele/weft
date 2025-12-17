package tui

import (
	"testing"
)

func TestParseMiB(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"123MiB", 123},
		{"80GiB", 80 * 1024},
		{"16G", 16 * 1024},
		{"128Gi", 128 * 1024},
		{"58.5G", int(58.5 * 1024)},
		{"0.5GiB", 512},
		{"", 0},
		{"unknown", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseMiB(tt.input)
			if got != tt.want {
				t.Errorf("parseMiB(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseHostInfo(t *testing.T) {
	output := `ARCH:Darwin arm64
OS:24.6.0
CPUS:8
LOAD:4.81 5.59 5.52
MEM:16G:-
MODEL:MacBookPro17,1
CPUMODEL:Apple M1
MACGPU:Chipset Model: Apple M1
MACGPU:Total Number of Cores: 8
MACGPU:Metal Support: Metal 3`

	host := ParseHostInfo(output)

	if host.Arch != "Darwin arm64" {
		t.Errorf("Arch = %q, want %q", host.Arch, "Darwin arm64")
	}
	if host.OS != "24.6.0" {
		t.Errorf("OS = %q, want %q", host.OS, "24.6.0")
	}
	if host.CPUs != 8 {
		t.Errorf("CPUs = %d, want %d", host.CPUs, 8)
	}
	if host.LoadAvg != "4.81 5.59 5.52" {
		t.Errorf("LoadAvg = %q, want %q", host.LoadAvg, "4.81 5.59 5.52")
	}
	if host.MemTotal != "16G" {
		t.Errorf("MemTotal = %q, want %q", host.MemTotal, "16G")
	}
	if host.MemUsed != "" {
		t.Errorf("MemUsed = %q, want %q", host.MemUsed, "")
	}
	if host.Model != "MacBookPro17,1" {
		t.Errorf("Model = %q, want %q", host.Model, "MacBookPro17,1")
	}
	if host.CPUModel != "Apple M1" {
		t.Errorf("CPUModel = %q, want %q", host.CPUModel, "Apple M1")
	}
	if len(host.GPUs) != 1 {
		t.Fatalf("len(GPUs) = %d, want %d", len(host.GPUs), 1)
	}
	if host.GPUs[0].Name != "Apple M1 (8 cores)" {
		t.Errorf("GPUs[0].Name = %q, want %q", host.GPUs[0].Name, "Apple M1 (8 cores)")
	}
}

func TestParseHostInfoLinux(t *testing.T) {
	output := `ARCH:Linux x86_64
OS:5.15.0-generic
CPUS:12
LOAD:0.5, 0.3, 0.2
MEM:128G:58G
GPUNAME:|   0  NVIDIA A100-SXM4-80GB   On   | 00000000:01:00.0 Off |                    0 |
GPUSTAT:| 30%   45C    P8    20W / 350W |    123MiB / 80000MiB |      5%      Default |`

	host := ParseHostInfo(output)

	if host.Arch != "Linux x86_64" {
		t.Errorf("Arch = %q, want %q", host.Arch, "Linux x86_64")
	}
	if host.CPUs != 12 {
		t.Errorf("CPUs = %d, want %d", host.CPUs, 12)
	}
	if host.MemTotal != "128G" {
		t.Errorf("MemTotal = %q, want %q", host.MemTotal, "128G")
	}
	if host.MemUsed != "58G" {
		t.Errorf("MemUsed = %q, want %q", host.MemUsed, "58G")
	}
	if len(host.GPUs) != 1 {
		t.Fatalf("len(GPUs) = %d, want %d", len(host.GPUs), 1)
	}
	if host.GPUs[0].Name != "NVIDIA A100-SXM4-80GB" {
		t.Errorf("GPUs[0].Name = %q, want %q", host.GPUs[0].Name, "NVIDIA A100-SXM4-80GB")
	}
	if host.GPUs[0].Temperature != 45 {
		t.Errorf("GPUs[0].Temperature = %d, want %d", host.GPUs[0].Temperature, 45)
	}
	if host.GPUs[0].Utilization != 5 {
		t.Errorf("GPUs[0].Utilization = %d, want %d", host.GPUs[0].Utilization, 5)
	}
	if host.GPUs[0].MemUsed != "123MiB" {
		t.Errorf("GPUs[0].MemUsed = %q, want %q", host.GPUs[0].MemUsed, "123MiB")
	}
}
