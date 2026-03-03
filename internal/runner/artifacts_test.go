package runner

import (
	"path/filepath"
	"testing"
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
