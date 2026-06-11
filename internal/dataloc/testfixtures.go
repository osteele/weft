package dataloc

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// WriteTestTorchPin writes a minimal uv.lock into dir that ScanTorchPin
// resolves to (version, cuVariant) — the torch package pinned from default
// PyPI with the matching nvidia-*-cu12 runtime package, the shape modern uv
// projects produce. Tests across packages share this fixture so the lockfile
// shape the parser expects is encoded once.
func WriteTestTorchPin(t *testing.T, dir, version, cuVariant string) {
	t.Helper()
	runtimeVersion := CUDAVariantVersion(cuVariant)
	if runtimeVersion == "" {
		t.Fatalf("WriteTestTorchPin: unrecognized CUDA variant %q", cuVariant)
	}
	lock := fmt.Sprintf(`
[[package]]
name = "torch"
version = %q
source = { registry = "https://pypi.org/simple" }

[[package]]
name = "nvidia-cuda-runtime-cu12"
version = "%s.90"
`, version, runtimeVersion)
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
}
