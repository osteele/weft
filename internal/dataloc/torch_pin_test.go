package dataloc

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestScanTorchPin_UVLockWheelURL(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "uv.lock"), `
[[package]]
name = "torch"
version = "2.6.0"
source = { registry = "https://download.pytorch.org/whl/cu128" }
wheels = [
    { url = "https://download.pytorch.org/whl/cu128/torch-2.6.0%2Bcu128-cp312-cp312-linux_x86_64.whl" },
]
`)
	pin := ScanTorchPin(dir)
	if pin == nil {
		t.Fatalf("expected pin, got nil")
	}
	if pin.Version != "2.6.0" {
		t.Errorf("Version = %q, want %q", pin.Version, "2.6.0")
	}
	if pin.CudaVariant != "cu128" {
		t.Errorf("CudaVariant = %q, want %q", pin.CudaVariant, "cu128")
	}
}

func TestScanTorchPin_UVLockNvidiaRuntime(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "uv.lock"), `
[[package]]
name = "torch"
version = "2.10.0"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/foo/torch-2.10.0-cp312-linux_x86_64.whl" },
]

[[package]]
name = "nvidia-cuda-runtime-cu12"
version = "12.8.90"
`)
	pin := ScanTorchPin(dir)
	if pin == nil {
		t.Fatalf("expected pin, got nil")
	}
	if pin.Version != "2.10.0" {
		t.Errorf("Version = %q", pin.Version)
	}
	if pin.CudaVariant != "cu128" {
		t.Errorf("CudaVariant = %q, want cu128", pin.CudaVariant)
	}
	if got := TorchMinCUDAVersion(dir); got != "12.8" {
		t.Errorf("TorchMinCUDAVersion = %q, want 12.8", got)
	}
}

func TestCUDAVariantVersion(t *testing.T) {
	tests := []struct {
		variant string
		want    string
	}{
		{"cu118", "11.8"},
		{"cu121", "12.1"},
		{"cu128", "12.8"},
		{"CU129", "12.9"},
		{"cpu", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := CUDAVariantVersion(tt.variant); got != tt.want {
			t.Errorf("CUDAVariantVersion(%q) = %q, want %q", tt.variant, got, tt.want)
		}
	}
}

func TestScanTorchPin_UVLockNvidiaCUDADeps(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "uv.lock"), `
[[package]]
name = "torch"
version = "2.6.0"
source = { registry = "https://pypi.org/simple" }

[[package]]
name = "nvidia-cublas-cu12"
version = "12.4.5.8"

[[package]]
name = "nvidia-cusparse-cu12"
version = "12.3.1.170"
`)
	pin := ScanTorchPin(dir)
	if pin == nil {
		t.Fatalf("expected pin, got nil")
	}
	if pin.Version != "2.6.0" {
		t.Errorf("Version = %q", pin.Version)
	}
	if pin.CudaVariant != "cu124" {
		t.Errorf("CudaVariant = %q, want cu124", pin.CudaVariant)
	}
	if got := TorchMaxComputeCap(pin.Version, pin.CudaVariant); got != "9.0" {
		t.Errorf("TorchMaxComputeCap(%q, %q) = %q, want 9.0", pin.Version, pin.CudaVariant, got)
	}
}

func TestScanTorchPin_PyprojectFallback(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "pyproject.toml"), `
[project]
name = "demo"
version = "0.1.0"
dependencies = [
    "torch==2.4.1",
    "transformers",
]

[tool.uv]
extra-index-url = ["https://download.pytorch.org/whl/cu121"]
`)
	pin := ScanTorchPin(dir)
	if pin == nil {
		t.Fatalf("expected pin, got nil")
	}
	if pin.Version != "2.4.1" {
		t.Errorf("Version = %q", pin.Version)
	}
	if pin.CudaVariant != "cu121" {
		t.Errorf("CudaVariant = %q, want cu121", pin.CudaVariant)
	}
}

func TestScanTorchPin_NoTorch(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "pyproject.toml"), `
[project]
name = "demo"
dependencies = ["requests"]
`)
	if pin := ScanTorchPin(dir); pin != nil {
		t.Errorf("expected nil pin, got %+v", pin)
	}
}

func TestScanTorchPin_WalkUp(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "src", "experiments")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "uv.lock"), `
[[package]]
name = "torch"
version = "2.5.1"

[[package]]
name = "nvidia-cuda-runtime-cu12"
version = "12.4.0"
`)
	pin := ScanTorchPin(sub)
	if pin == nil {
		t.Fatalf("expected to find pin via walk-up")
	}
	if pin.Version != "2.5.1" || pin.CudaVariant != "cu124" {
		t.Errorf("got %+v", pin)
	}
}

func TestCUDARuntimePackages(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "uv.lock")
	mustWrite(t, lock, `
[[package]]
name = "torch"
version = "2.6.0"

[[package]]
name = "numpy"
version = "2.1.0"

[[package]]
name = "nvidia-cusparse-cu12"
version = "12.3.1.170"

[[package]]
name = "nvidia-nvjitlink-cu12"
version = "12.4.127"

[[package]]
name = "nvidia-cudnn-cu12"
version = "9.1.0.70"

[[package]]
name = "nvidia-ml-py"
version = "13.590.48"

[[package]]
name = "triton"
version = "3.2.0"
`)
	got := CUDARuntimePackages(lock)
	// nvidia-*-cu12 libs and triton are returned; torch, numpy, and the
	// pure-Python nvidia-ml-py (no -cu12 suffix) are not.
	want := []string{"nvidia-cusparse-cu12", "nvidia-nvjitlink-cu12", "nvidia-cudnn-cu12", "triton"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("CUDARuntimePackages = %v, want %v", got, want)
	}
}

func TestCUDARuntimePackages_MissingFile(t *testing.T) {
	if got := CUDARuntimePackages(filepath.Join(t.TempDir(), "uv.lock")); got != nil {
		t.Errorf("expected nil for missing uv.lock, got %v", got)
	}
}

func TestImageProvidedTorchPackages(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "uv.lock")
	mustWrite(t, lock, `
[[package]]
name = "torch"
version = "2.6.0"

[[package]]
name = "nvidia-cusparse-cu12"
version = "12.3.1.170"
`)
	got := ImageProvidedTorchPackages(lock)
	for _, want := range []string{"torch", "torchaudio", "torchvision", "nvidia-cusparse-cu12"} {
		if !slices.Contains(got, want) {
			t.Errorf("ImageProvidedTorchPackages missing %q; got %v", want, got)
		}
	}

	// A missing lockfile still yields the torch base packages.
	if bare := ImageProvidedTorchPackages(filepath.Join(dir, "absent.lock")); len(bare) != 3 {
		t.Errorf("ImageProvidedTorchPackages(missing) = %v, want 3 torch packages", bare)
	}
}

func TestUVNoInstallPackageFlags(t *testing.T) {
	if got := UVNoInstallPackageFlags(nil); got != "" {
		t.Errorf("UVNoInstallPackageFlags(nil) = %q, want empty", got)
	}
	got := UVNoInstallPackageFlags([]string{"torch", "nvidia-cusparse-cu12"})
	want := " --no-install-package torch --no-install-package nvidia-cusparse-cu12"
	if got != want {
		t.Errorf("UVNoInstallPackageFlags = %q, want %q", got, want)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
