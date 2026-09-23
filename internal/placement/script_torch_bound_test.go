package placement

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/imagereq"
)

// writeScriptTorchProject builds the wb166 shape: a project whose uv.lock
// resolves torch 2.12 (cu128), and a PEP 723 script with its own torch
// requirement that `uv run` installs in the script's environment instead.
func writeScriptTorchProject(t *testing.T, torchReq string) (dir, command string) {
	t.Helper()
	dir = t.TempDir()
	writePyproject(t, dir, "[project]\ndependencies = [\"torch>=2.0\"]\n")
	dataloc.WriteTestTorchPin(t, dir, "2.12.1", "cu128")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(dir, "scripts", "exp.py"), "# /// script\n# dependencies = [\""+torchReq+"\", \"transformers>=4.56,<5\"]\n# ///\nimport torch\n")
	return dir, "uv run --locked scripts/exp.py --phase pilot"
}

func TestScriptTorchUpperBoundDeterminesCapAndFloor(t *testing.T) {
	dir, command := writeScriptTorchProject(t, "torch>=2.2,<2.7")

	if got := ResolveJobMaxComputeCapForPersistence(dir, command); got != "9.0" {
		t.Errorf("cap = %q, want 9.0 (torch 2.6 ships kernels through Hopper)", got)
	}

	exact, err := ResolveEffectiveRuntime(dir, command, EffectiveRuntimeOptions{FloorMode: RuntimeFloorExact})
	if err != nil {
		t.Fatalf("ResolveEffectiveRuntime exact: %v", err)
	}
	if exact.Floor.Req.MinCUDAVersion != "12.4" {
		t.Errorf("exact MinCUDAVersion = %q, want 12.4 (torch 2.6 default cu124 wheel)", exact.Floor.Req.MinCUDAVersion)
	}
	if want := imagereq.MinDriverForCUDA("12.4"); exact.Floor.Req.MinDriverVersion != want {
		t.Errorf("exact MinDriverVersion = %d, want %d", exact.Floor.Req.MinDriverVersion, want)
	}

	family, err := ResolveEffectiveRuntime(dir, command, EffectiveRuntimeOptions{FloorMode: RuntimeFloorFamily})
	if err != nil {
		t.Fatalf("ResolveEffectiveRuntime family: %v", err)
	}
	if family.Floor.Req.MinCUDAVersion != "12.0" {
		t.Errorf("family MinCUDAVersion = %q, want 12.0 (cu124 is in the CUDA 12.x family)", family.Floor.Req.MinCUDAVersion)
	}
}

// Each `uv run script.py` step resolves its own environment, so a later
// torch 2.9.1 (cu128) step must keep its 12.8 operational floor even though an
// earlier step's bounded range resolves an older wheel.
func TestScriptTorchFloorsMergeAcrossCompoundCommandSteps(t *testing.T) {
	dir, _ := writeScriptTorchProject(t, "torch>=2.2,<2.7")
	writeScript(t, filepath.Join(dir, "scripts", "second.py"), "# /// script\n# dependencies = [\"torch==2.9.1\"]\n# ///\nimport torch\n")
	command := "uv run scripts/exp.py && uv run scripts/second.py"

	for _, mode := range []RuntimeFloorMode{RuntimeFloorExact, RuntimeFloorFamily} {
		runtime, err := ResolveEffectiveRuntime(dir, command, EffectiveRuntimeOptions{FloorMode: mode})
		if err != nil {
			t.Fatalf("ResolveEffectiveRuntime mode %d: %v", mode, err)
		}
		if runtime.Floor.Req.MinCUDAVersion != "12.8" {
			t.Errorf("mode %d MinCUDAVersion = %q, want 12.8 from the torch 2.9.1+cu128 step", mode, runtime.Floor.Req.MinCUDAVersion)
		}
		if want := imagereq.MinDriverForCUDA("12.8"); runtime.Floor.Req.MinDriverVersion != want {
			t.Errorf("mode %d MinDriverVersion = %d, want %d", mode, runtime.Floor.Req.MinDriverVersion, want)
		}
	}
}

// A `uv run python` step imports the project lock's torch 2.12.1+cu128 even
// though another step runs a PEP 723 environment resolving an older wheel, so
// the project wheel's 12.8 floor must survive the max-merge.
func TestScriptTorchFloorsIncludeProjectEnvSteps(t *testing.T) {
	dir, _ := writeScriptTorchProject(t, "torch>=2.2,<2.7")
	writeScript(t, filepath.Join(dir, "scripts", "prep.py"), "import torch\n")
	command := "uv run python scripts/prep.py && uv run scripts/exp.py"

	for _, mode := range []RuntimeFloorMode{RuntimeFloorExact, RuntimeFloorFamily} {
		runtime, err := ResolveEffectiveRuntime(dir, command, EffectiveRuntimeOptions{FloorMode: mode})
		if err != nil {
			t.Fatalf("ResolveEffectiveRuntime mode %d: %v", mode, err)
		}
		if runtime.Floor.Req.MinCUDAVersion != "12.8" {
			t.Errorf("mode %d MinCUDAVersion = %q, want 12.8 from the project torch 2.12.1+cu128 step", mode, runtime.Floor.Req.MinCUDAVersion)
		}
	}
}

// Operators inside quotes are arguments, not steps, so they must not add a
// project-environment step and pull in the project wheel's 12.8 floor.
func TestScriptTorchFloorsIgnoreQuotedOperators(t *testing.T) {
	dir, _ := writeScriptTorchProject(t, "torch>=2.2,<2.7")
	command := `echo 'start; ready' && uv run scripts/exp.py --tag 'a|python b'`

	want := map[RuntimeFloorMode]string{RuntimeFloorExact: "12.4", RuntimeFloorFamily: "12.0"}
	for mode, cuda := range want {
		runtime, err := ResolveEffectiveRuntime(dir, command, EffectiveRuntimeOptions{FloorMode: mode})
		if err != nil {
			t.Fatalf("ResolveEffectiveRuntime mode %d: %v", mode, err)
		}
		if runtime.Floor.Req.MinCUDAVersion != cuda {
			t.Errorf("mode %d MinCUDAVersion = %q, want %s from the script's torch 2.6 wheel alone", mode, runtime.Floor.Req.MinCUDAVersion, cuda)
		}
	}
}

func TestScriptTorchOpenRangeKeepsLatestTorchFloor(t *testing.T) {
	dir, command := writeScriptTorchProject(t, "torch>=2.2")

	// An open script range does not determine a release, so the cap still
	// comes from the project torch pin.
	if got := ResolveJobMaxComputeCapForPersistence(dir, command); got != "12.0" {
		t.Errorf("cap = %q, want 12.0 from the project torch 2.12 pin", got)
	}
	for _, mode := range []RuntimeFloorMode{RuntimeFloorExact, RuntimeFloorFamily} {
		runtime, err := ResolveEffectiveRuntime(dir, command, EffectiveRuntimeOptions{FloorMode: mode})
		if err != nil {
			t.Fatalf("ResolveEffectiveRuntime mode %d: %v", mode, err)
		}
		if runtime.Floor.Req.MinCUDAVersion != dataloc.OpenEndedTorchCUDAFloor {
			t.Errorf("mode %d MinCUDAVersion = %q, want open-range floor %q", mode, runtime.Floor.Req.MinCUDAVersion, dataloc.OpenEndedTorchCUDAFloor)
		}
	}
}
