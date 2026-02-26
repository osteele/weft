package inventory

import (
	"testing"
)

func TestLoadEmbeddedHosts(t *testing.T) {
	hosts, err := LoadEmbeddedHosts()
	if err != nil {
		t.Fatalf("LoadEmbeddedHosts: %v", err)
	}
	if len(hosts) < 3 {
		t.Fatalf("expected at least 3 hosts, got %d", len(hosts))
	}

	// Verify each host has required fields
	for _, h := range hosts {
		if h.Name == "" {
			t.Error("host has empty name")
		}
		if h.OS == "" {
			t.Errorf("host %s has empty OS", h.Name)
		}
		if h.Arch == "" {
			t.Errorf("host %s has empty Arch", h.Name)
		}
		if len(h.GPUs) == 0 {
			t.Errorf("host %s has no GPUs", h.Name)
		}
		if h.CPUCores <= 0 {
			t.Errorf("host %s has invalid CPUCores: %d", h.Name, h.CPUCores)
		}
		if h.Memory == "" {
			t.Errorf("host %s has empty Memory", h.Name)
		}
	}
}

func TestLoadEmbeddedHosts_Deterministic(t *testing.T) {
	hosts1, err := LoadEmbeddedHosts()
	if err != nil {
		t.Fatal(err)
	}
	hosts2, err := LoadEmbeddedHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts1) != len(hosts2) {
		t.Fatalf("non-deterministic: got %d then %d hosts", len(hosts1), len(hosts2))
	}
	for i := range hosts1 {
		if hosts1[i].Name != hosts2[i].Name {
			t.Errorf("non-deterministic order: index %d got %s then %s", i, hosts1[i].Name, hosts2[i].Name)
		}
	}
}

func TestLoadEmbeddedHosts_Cool100(t *testing.T) {
	hosts, err := LoadEmbeddedHosts()
	if err != nil {
		t.Fatalf("LoadEmbeddedHosts: %v", err)
	}

	var cool100 *HostSpec
	for i := range hosts {
		if hosts[i].Name == "cool100" {
			cool100 = &hosts[i]
			break
		}
	}
	if cool100 == nil {
		t.Fatal("cool100 not found in embedded hosts")
	}

	if cool100.OS != "linux" || cool100.Arch != "amd64" {
		t.Errorf("cool100: got os=%s arch=%s, want linux/amd64", cool100.OS, cool100.Arch)
	}
	if cool100.CPUCores != 64 {
		t.Errorf("cool100 CPUCores: got %d, want 64", cool100.CPUCores)
	}
	if cool100.Memory != "256GB" {
		t.Errorf("cool100 Memory: got %s, want 256GB", cool100.Memory)
	}
	if len(cool100.GPUs) != 2 {
		t.Fatalf("cool100: got %d GPU groups, want 2", len(cool100.GPUs))
	}
	if cool100.TotalGPUs() != 10 {
		t.Errorf("cool100: got %d total GPUs, want 10", cool100.TotalGPUs())
	}

	// Verify A100 group
	a100 := cool100.GPUs[0]
	if a100.Class != "a100" {
		t.Errorf("cool100 GPU[0] class: got %s, want a100", a100.Class)
	}
	if a100.Memory != "80GB" {
		t.Errorf("cool100 GPU[0] memory: got %s, want 80GB", a100.Memory)
	}
	if len(a100.Indices) != 2 {
		t.Errorf("cool100 GPU[0] indices: got %d, want 2", len(a100.Indices))
	}

	// Verify 2080 Ti group
	rtx2080 := cool100.GPUs[1]
	if rtx2080.Class != "rtx2080ti" {
		t.Errorf("cool100 GPU[1] class: got %s, want rtx2080ti", rtx2080.Class)
	}
	if len(rtx2080.Indices) != 8 {
		t.Errorf("cool100 GPU[1] indices: got %d, want 8", len(rtx2080.Indices))
	}
}

func TestLoadEmbeddedHosts_Cool30(t *testing.T) {
	spec := FindHost("cool30")
	if spec == nil {
		t.Fatal("cool30 not found")
	}
	if spec.OS != "linux" || spec.Arch != "amd64" {
		t.Errorf("cool30: got os=%s arch=%s, want linux/amd64", spec.OS, spec.Arch)
	}
	if spec.CPUCores != 16 {
		t.Errorf("cool30 CPUCores: got %d, want 16", spec.CPUCores)
	}
	if len(spec.GPUs) != 1 {
		t.Fatalf("cool30: got %d GPU groups, want 1", len(spec.GPUs))
	}
	if spec.GPUs[0].Class != "rtx3090" {
		t.Errorf("cool30 GPU class: got %s, want rtx3090", spec.GPUs[0].Class)
	}
	if spec.GPUs[0].Memory != "24GB" {
		t.Errorf("cool30 GPU memory: got %s, want 24GB", spec.GPUs[0].Memory)
	}
}

func TestLoadEmbeddedHosts_Studio(t *testing.T) {
	spec := FindHost("studio")
	if spec == nil {
		t.Fatal("studio not found")
	}
	if spec.OS != "darwin" || spec.Arch != "arm64" {
		t.Errorf("studio: got os=%s arch=%s, want darwin/arm64", spec.OS, spec.Arch)
	}
	if spec.CPUCores != 12 {
		t.Errorf("studio CPUCores: got %d, want 12", spec.CPUCores)
	}
	if len(spec.GPUs) != 1 {
		t.Fatalf("studio: got %d GPU groups, want 1", len(spec.GPUs))
	}
	if spec.GPUs[0].Class != "m2max" {
		t.Errorf("studio GPU class: got %s, want m2max", spec.GPUs[0].Class)
	}
}

func TestFindHost(t *testing.T) {
	spec := FindHost("cool30")
	if spec == nil {
		t.Fatal("FindHost(cool30) returned nil")
	}
	if spec.OS != "linux" {
		t.Errorf("cool30 OS: got %s, want linux", spec.OS)
	}

	if FindHost("nonexistent") != nil {
		t.Error("FindHost(nonexistent) should return nil")
	}
}

func TestFindHost_AllHosts(t *testing.T) {
	for _, name := range []string{"cool30", "cool100", "studio"} {
		spec := FindHost(name)
		if spec == nil {
			t.Errorf("FindHost(%s) returned nil", name)
			continue
		}
		if spec.Name != name {
			t.Errorf("FindHost(%s).Name = %s", name, spec.Name)
		}
	}
}

func TestFindHost_ReturnsCopy(t *testing.T) {
	// Verify FindHost returns independent results
	s1 := FindHost("cool30")
	s2 := FindHost("cool30")
	if s1 == s2 {
		t.Error("FindHost should return independent copies, got same pointer")
	}
}

func TestHostSpec_TotalGPUs(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{"cool30", 1},
		{"cool100", 10},
		{"studio", 1},
	}
	for _, tt := range tests {
		spec := FindHost(tt.name)
		if spec == nil {
			t.Fatalf("FindHost(%s) returned nil", tt.name)
		}
		if got := spec.TotalGPUs(); got != tt.want {
			t.Errorf("%s.TotalGPUs() = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestHostSpec_TotalGPUs_Empty(t *testing.T) {
	spec := HostSpec{Name: "empty"}
	if got := spec.TotalGPUs(); got != 0 {
		t.Errorf("empty host TotalGPUs() = %d, want 0", got)
	}
}

func TestHostSpec_TotalGPUs_MultiGroup(t *testing.T) {
	spec := HostSpec{
		Name: "test",
		GPUs: []GPUSpec{
			{Indices: []int{0, 1}},
			{Indices: []int{2, 3, 4}},
		},
	}
	if got := spec.TotalGPUs(); got != 5 {
		t.Errorf("TotalGPUs() = %d, want 5", got)
	}
}

func TestGPUSpec_Fields(t *testing.T) {
	spec := FindHost("cool100")
	if spec == nil {
		t.Fatal("cool100 not found")
	}

	for _, gpu := range spec.GPUs {
		if gpu.Name == "" {
			t.Error("GPU has empty Name")
		}
		if gpu.Class == "" {
			t.Error("GPU has empty Class")
		}
		if gpu.Memory == "" {
			t.Error("GPU has empty Memory")
		}
		if len(gpu.Indices) == 0 {
			t.Error("GPU has empty Indices")
		}
	}
}

func TestGPUIndicesAreSequential(t *testing.T) {
	spec := FindHost("cool100")
	if spec == nil {
		t.Fatal("cool100 not found")
	}

	// A100s should be indices 0,1
	a100 := spec.GPUs[0]
	if a100.Indices[0] != 0 || a100.Indices[1] != 1 {
		t.Errorf("A100 indices: got %v, want [0, 1]", a100.Indices)
	}

	// 2080 Tis should be indices 2-9
	rtx := spec.GPUs[1]
	for i, idx := range rtx.Indices {
		if idx != i+2 {
			t.Errorf("RTX 2080 Ti index %d: got %d, want %d", i, idx, i+2)
		}
	}
}

func TestAllHostsHaveValidOSArch(t *testing.T) {
	validOS := map[string]bool{"linux": true, "darwin": true}
	validArch := map[string]bool{"amd64": true, "arm64": true}

	hosts, err := LoadEmbeddedHosts()
	if err != nil {
		t.Fatal(err)
	}

	for _, h := range hosts {
		if !validOS[h.OS] {
			t.Errorf("host %s has unexpected OS: %s", h.Name, h.OS)
		}
		if !validArch[h.Arch] {
			t.Errorf("host %s has unexpected Arch: %s", h.Name, h.Arch)
		}
	}
}

func TestHostNamesAreUnique(t *testing.T) {
	hosts, err := LoadEmbeddedHosts()
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	for _, h := range hosts {
		if seen[h.Name] {
			t.Errorf("duplicate host name: %s", h.Name)
		}
		seen[h.Name] = true
	}
}
