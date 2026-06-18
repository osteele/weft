package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/config"
)

func TestLoadHostsFromDir(t *testing.T) {
	dir := t.TempDir()
	yaml1 := `name: myhost
os: linux
arch: amd64
cpu_cores: 8
memory: 32GB
gpus:
  - name: RTX 3090
    class: rtx3090
    memory: 24GB
    indices: [0]
`
	if err := os.WriteFile(filepath.Join(dir, "myhost.yaml"), []byte(yaml1), 0644); err != nil {
		t.Fatal(err)
	}

	hosts, err := LoadHostsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadHostsFromDir: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1", len(hosts))
	}
	if hosts[0].Name != "myhost" {
		t.Errorf("name = %q, want myhost", hosts[0].Name)
	}
	if hosts[0].CPUCores != 8 {
		t.Errorf("cpu_cores = %d, want 8", hosts[0].CPUCores)
	}
	if hosts[0].Memory != "32GB" {
		t.Errorf("memory = %q, want 32GB", hosts[0].Memory)
	}
	if len(hosts[0].GPUs) != 1 {
		t.Fatalf("got %d GPU groups, want 1", len(hosts[0].GPUs))
	}
	if hosts[0].GPUs[0].Class != "rtx3090" {
		t.Errorf("gpu class = %q, want rtx3090", hosts[0].GPUs[0].Class)
	}
}

func TestLoadHostsFromDir_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		yaml := "name: " + name + "\nos: linux\narch: amd64\ncpu_cores: 4\nmemory: 16GB\n"
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
	}

	hosts, err := LoadHostsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadHostsFromDir: %v", err)
	}
	if len(hosts) != 3 {
		t.Fatalf("got %d hosts, want 3", len(hosts))
	}
}

func TestLoadHostsFromDir_SkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.yaml"), []byte("name: h1\nos: linux\narch: amd64\n"), 0644); err != nil {
		t.Fatal(err)
	}

	hosts, err := LoadHostsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadHostsFromDir: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1", len(hosts))
	}
}

func TestLoadHostsFromDir_MissingDir(t *testing.T) {
	_, err := LoadHostsFromDir("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestLoadHosts_MissingDir(t *testing.T) {
	// LoadHosts returns empty slice when dir doesn't exist
	// We can't easily override HostsDir, but we can test LoadHostsFromDir behavior
	// indirectly — LoadHosts wraps LoadHostsFromDir with ErrNotExist handling.
	hosts, err := LoadHostsFromDir(t.TempDir()) // empty dir
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("got %d hosts from empty dir, want 0", len(hosts))
	}
}

func TestLoadHostsFromDir_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("{{{\tinvalid"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadHostsFromDir(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestHostsDirUsesConfigDir(t *testing.T) {
	dir := t.TempDir()
	restoreConfig := config.SetConfigPathsForTesting(filepath.Join(dir, "config.toml"), filepath.Join(dir, "config.yaml"))
	t.Cleanup(restoreConfig)
	restoreHostsDir := SetHostsDir("")
	t.Cleanup(restoreHostsDir)

	want := filepath.Join(dir, "hosts")
	if got := HostsDir(); got != want {
		t.Fatalf("HostsDir() = %q, want %q", got, want)
	}
}

func TestLoadHosts_MergesConfigAndDiscoveredHosts(t *testing.T) {
	dir := t.TempDir()
	hostsDir := filepath.Join(dir, "hosts")
	if err := os.MkdirAll(hostsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostsDir, "cool30.yaml"), []byte(`
name: cool30
os: linux
arch: amd64
cpu_cores: 16
memory: 64GB
`), 0o644); err != nil {
		t.Fatal(err)
	}

	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[hosts.cool100]
os = "darwin"
arch = "arm64"
cpu_cores = 12
memory = "96GB"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	restoreConfig := config.SetConfigPathsForTesting(tomlPath, filepath.Join(dir, "config.yaml"))
	t.Cleanup(restoreConfig)
	restoreHostsDir := SetHostsDir(hostsDir)
	t.Cleanup(restoreHostsDir)

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatalf("LoadHosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want 2", len(hosts))
	}
	if FindHost("cool30") == nil {
		t.Fatal("expected discovered host cool30 to be present")
	}
	if FindHost("cool100") == nil {
		t.Fatal("expected config host cool100 to be present")
	}
}

func TestLoadHosts_ConfigOverridesDiscoveredHost(t *testing.T) {
	dir := t.TempDir()
	hostsDir := filepath.Join(dir, "hosts")
	if err := os.MkdirAll(hostsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostsDir, "cool30.yaml"), []byte(`
name: cool30
os: linux
arch: amd64
cpu_cores: 16
memory: 64GB
`), 0o644); err != nil {
		t.Fatal(err)
	}

	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[hosts.cool30]
os = "linux"
arch = "amd64"
cpu_cores = 32
memory = "128GB"

[hosts.cool30.benchmark]
cpu_threshold = 15
ram_threshold = 35
`), 0o644); err != nil {
		t.Fatal(err)
	}

	restoreConfig := config.SetConfigPathsForTesting(tomlPath, filepath.Join(dir, "config.yaml"))
	t.Cleanup(restoreConfig)
	restoreHostsDir := SetHostsDir(hostsDir)
	t.Cleanup(restoreHostsDir)

	host := FindHost("cool30")
	if host == nil {
		t.Fatal("expected cool30 to be present")
	}
	if host.CPUCores != 32 {
		t.Fatalf("cpu_cores = %d, want 32", host.CPUCores)
	}
	if host.Memory != "128GB" {
		t.Fatalf("memory = %q, want 128GB", host.Memory)
	}
	if host.Benchmark.CPUThreshold != 15 {
		t.Fatalf("benchmark cpu_threshold = %d, want 15", host.Benchmark.CPUThreshold)
	}
	if host.Benchmark.RAMThreshold != 35 {
		t.Fatalf("benchmark ram_threshold = %d, want 35", host.Benchmark.RAMThreshold)
	}
	if got := host.BenchmarkEnvPrefix(); got != "WEFT_BENCHMARK_CPU=15 WEFT_BENCHMARK_RAM=35 " {
		t.Fatalf("BenchmarkEnvPrefix = %q", got)
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

func TestParseNetworkBW(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"1Gbps", 1e9 / 8},
		{"10Gbps", 10e9 / 8},
		{"100Mbps", 100e6 / 8},
		{"", 0},
		{"garbage", 0},
	}
	for _, tt := range tests {
		got := ParseNetworkBW(tt.input)
		if got != tt.want {
			t.Errorf("ParseNetworkBW(%q) = %g, want %g", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeGPUClass(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"RTX 3090", "rtx3090"},
		{"rtx3090", "rtx3090"},
		{"rtx-3090", "rtx3090"},
		{"3090", "rtx3090"},
		{"2080ti", "rtx2080ti"},
		{"2080 Ti", "rtx2080ti"},
		{"4090", "rtx4090"},
		{"A100", "a100"},
		{"M2 Max", "m2max"},
	}
	for _, tt := range tests {
		got := NormalizeGPUClass(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeGPUClass(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTestHosts_RequiredFields(t *testing.T) {
	hosts := TestHosts()
	if len(hosts) < 3 {
		t.Fatalf("expected at least 3 test hosts, got %d", len(hosts))
	}

	for _, h := range hosts {
		if h.Name == "" {
			t.Error("test host has empty name")
		}
		if h.OS == "" {
			t.Errorf("test host %s has empty OS", h.Name)
		}
		if h.Arch == "" {
			t.Errorf("test host %s has empty Arch", h.Name)
		}
		if h.CPUCores <= 0 {
			t.Errorf("test host %s has invalid CPUCores: %d", h.Name, h.CPUCores)
		}
		if h.Memory == "" {
			t.Errorf("test host %s has empty Memory", h.Name)
		}
		if len(h.GPUs) == 0 {
			t.Errorf("test host %s has no GPUs", h.Name)
		}
	}
}

func TestTestHosts_UniqueNames(t *testing.T) {
	seen := make(map[string]bool)
	for _, h := range TestHosts() {
		if seen[h.Name] {
			t.Errorf("duplicate test host name: %s", h.Name)
		}
		seen[h.Name] = true
	}
}

func TestFindTestHost(t *testing.T) {
	if h := FindTestHost("host-alpha"); h == nil {
		t.Error("FindTestHost(host-alpha) returned nil")
	}
	if h := FindTestHost("nonexistent"); h != nil {
		t.Error("FindTestHost(nonexistent) should return nil")
	}
}
