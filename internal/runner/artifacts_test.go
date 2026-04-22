package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/artifacts"
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
		})
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
