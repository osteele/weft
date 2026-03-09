package inventory

import (
	"os"
	"path/filepath"
	"testing"
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
