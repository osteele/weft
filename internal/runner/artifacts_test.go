package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/opsqueue"
)

func TestParseProducesSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		path    string
		version int64
	}{
		{"path only", "output/model.pt", "output/model.pt", 0},
		{"with version", "output/model.pt:100", "output/model.pt", 100},
		{"no version suffix", "results/checkpoint.bin", "results/checkpoint.bin", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ParseProducesSpec(tt.spec)
			if spec.Path != tt.path {
				t.Errorf("path = %q, want %q", spec.Path, tt.path)
			}
			if spec.Version != tt.version {
				t.Errorf("version = %d, want %d", spec.Version, tt.version)
			}
		})
	}
}

func TestParseNeedsSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		path    string
		version int64
		wantErr bool
	}{
		{"valid", "output/model.pt:100", "output/model.pt", 100, false},
		{"directory target", "output/checkpoints/:100", "", 0, true},
		{"no version", "output/model.pt", "", 0, true},
		{"invalid version", "output/model.pt:abc", "", 0, true},
		{"zero version", "output/model.pt:0", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := ParseNeedsSpec(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if spec.Path != tt.path {
				t.Errorf("path = %q, want %q", spec.Path, tt.path)
			}
			if spec.Version != tt.version {
				t.Errorf("version = %d, want %d", spec.Version, tt.version)
			}
			if spec.IsAsset() {
				t.Errorf("IsAsset() = true for non-asset spec %q", tt.spec)
			}
		})
	}
}

// TestValidateNeedsSpecsRejectsDirectoryTarget is the regression test for
// directory needs entering the cloud staging retry loop. Downstream staging
// resolves one concrete object, so accepting a trailing slash here creates a
// launch record that can never reach provider creation.
func TestValidateNeedsSpecsRejectsDirectoryTarget(t *testing.T) {
	err := ValidateNeedsSpecs([]string{"output/exp_154/:6156"})
	if err == nil {
		t.Fatal("expected directory needs target to be rejected")
	}
	if got, want := err.Error(), `needs spec "output/exp_154/:6156" names a directory; --needs requires a single file`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestParseNeedsSpec_Asset(t *testing.T) {
	t.Run("valid asset", func(t *testing.T) {
		spec, err := ParseNeedsSpec("asset:exp207-eval-llama8b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !spec.IsAsset() {
			t.Fatalf("IsAsset() = false; want true")
		}
		if spec.AssetName != "exp207-eval-llama8b" {
			t.Errorf("AssetName = %q, want %q", spec.AssetName, "exp207-eval-llama8b")
		}
		if spec.Version != 0 {
			t.Errorf("Version = %d, want 0 for asset form", spec.Version)
		}
	})
	t.Run("empty asset name", func(t *testing.T) {
		if _, err := ParseNeedsSpec("asset:"); err == nil {
			t.Fatalf("expected error for asset: with empty name")
		}
	})
}

func TestValidateNeedsSpecsRejectsWJPrefix(t *testing.T) {
	err := ValidateNeedsSpecs([]string{"output/nsweep_reps.tar:wj5070"})
	if err == nil {
		t.Fatal("expected malformed wj-prefixed needs spec to be rejected")
	}
	if got, want := err.Error(), `needs spec "output/nsweep_reps.tar:wj5070" must include :version suffix or use asset:NAME form`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestArtifactSatisfiedFile(t *testing.T) {
	tests := []struct {
		name     string
		logDir   string
		path     string
		version  int64
		expected string
	}{
		{"nested path", "/tmp/logs", "output/model.pt", 100, filepath.Join("/tmp/logs", "artifact-100-output%2Fmodel.pt.satisfied")},
		{"simple path", "/tmp/logs", "model.pt", 42, filepath.Join("/tmp/logs", "artifact-42-model.pt.satisfied")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ArtifactSatisfiedFile(tt.logDir, tt.path, tt.version)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestRecordProducedArtifacts_WritesManifestEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	jobID := int64(123)

	if err := RecordProducedArtifacts(jobID, []string{
		"cache/model.bin",
		"output/result.json:456",
	}); err != nil {
		t.Fatalf("RecordProducedArtifacts: %v", err)
	}

	manifestPath := filepath.Join(home, ".cache", "weft", "artifacts", "123.json")
	manifest, err := artifacts.ReadManifestFile(manifestPath, jobID)
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	if got, want := len(manifest.Artifacts), 2; got != want {
		t.Fatalf("artifact count = %d, want %d", got, want)
	}
	if manifest.Artifacts[0].Path != "cache/model.bin" {
		t.Fatalf("first artifact path = %q, want %q", manifest.Artifacts[0].Path, "cache/model.bin")
	}
	if manifest.Artifacts[1].Path != "output/result.json" {
		t.Fatalf("second artifact path = %q, want %q", manifest.Artifacts[1].Path, "output/result.json")
	}
}

// Regression: --produces declarations must be visible in the manifest BEFORE
// the script runs (so the periodic 60s output uploader can pick up large
// in-progress files like checkpoints), not only after exit. We exercise the
// full RunSingleJob path with a script that sleeps long enough that the
// manifest must be present before the script writes its files.
func TestRunSingleJob_PreRegistersProducesBeforeRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logDir := t.TempDir()
	workDir := t.TempDir()
	jobID := int64(7777)

	manifestPath := filepath.Join(home, ".cache", "weft", "artifacts", "7777.json")

	// Script: assert the manifest already contains both --produces entries
	// before doing any work. If pre-registration regresses, the script will
	// fail and the test will catch it.
	cmd := `python3 -c '
import json, os, sys
m = json.load(open(os.environ["WEFT_ARTIFACT_MANIFEST"]))
paths = sorted(a["path"] for a in m["artifacts"])
expected = ["output/ckpt.pt", "output/results.json"]
assert paths == expected, f"manifest paths = {paths!r}, want {expected!r}"
'`

	cfg := SingleJobConfig{
		JobID: jobID,
		Job: opsqueue.CommandJob{
			Cmd:      cmd,
			Produces: []string{"output/ckpt.pt", "output/results.json"},
		},
		LogDir:         logDir,
		WorkingDir:     workDir,
		SampleInterval: 100 * time.Millisecond,
		SkipProbes:     true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		// Surface log to help debugging if the assertion failed.
		paths := NewJobPaths(logDir, jobID)
		logData, _ := os.ReadFile(paths.Log)
		t.Fatalf("script exit %d (manifest pre-registration regression?). log:\n%s", ei.ExitCode, logData)
	}

	manifest, err := artifacts.ReadManifestFile(manifestPath, jobID)
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	if got, want := len(manifest.Artifacts), 2; got != want {
		t.Fatalf("artifact count = %d, want %d (manifest=%+v)", got, want, manifest.Artifacts)
	}
}

func TestRecordProducedArtifacts_MergesExistingManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	jobID := int64(124)
	manifestPath := filepath.Join(home, ".cache", "weft", "artifacts", "124.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	seed := artifacts.Manifest{
		JobID:     jobID,
		Artifacts: []artifacts.ArtifactSpec{{Path: "output/already.json"}},
	}
	if err := artifacts.WriteManifestFile(manifestPath, seed); err != nil {
		t.Fatalf("WriteManifestFile: %v", err)
	}

	if err := RecordProducedArtifacts(jobID, []string{
		"output/already.json",
		"output/new.json",
	}); err != nil {
		t.Fatalf("RecordProducedArtifacts: %v", err)
	}

	manifest, err := artifacts.ReadManifestFile(manifestPath, jobID)
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	if got, want := len(manifest.Artifacts), 2; got != want {
		t.Fatalf("artifact count = %d, want %d", got, want)
	}
	if manifest.Artifacts[0].Path != "output/already.json" || manifest.Artifacts[1].Path != "output/new.json" {
		t.Fatalf("artifact paths = %+v", manifest.Artifacts)
	}
}

func TestRecordDeclaredArtifacts_IncludesLocalOutputs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	jobID := int64(125)

	if err := RecordDeclaredArtifacts(jobID, []string{"output/model.pt:456"}, []string{
		"local:output/exp-231/",
		"checkpoint:phase-residual",
	}); err != nil {
		t.Fatalf("RecordDeclaredArtifacts: %v", err)
	}

	manifestPath := filepath.Join(home, ".cache", "weft", "artifacts", "125.json")
	manifest, err := artifacts.ReadManifestFile(manifestPath, jobID)
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	paths := make(map[string]bool, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		paths[artifact.Path] = true
	}
	if !paths["output/model.pt"] {
		t.Fatalf("produces path missing from manifest: %+v", manifest.Artifacts)
	}
	if !paths["output/exp-231/"] {
		t.Fatalf("local output path missing from manifest: %+v", manifest.Artifacts)
	}
	if paths["checkpoint:phase-residual"] {
		t.Fatalf("non-filesystem data output was added to manifest: %+v", manifest.Artifacts)
	}
}
