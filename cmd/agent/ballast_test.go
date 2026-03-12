package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentJobIDFromPhase(t *testing.T) {
	tests := []struct {
		phase string
		want  int64
	}{
		{"running:123", 123},
		{"finalizing:66", 66},
		{"uploading:77", 77},
		{"disk-full:88", 88},
		{"grace", 0},
		{"running:not-a-number", 0},
	}
	for _, tt := range tests {
		if got := currentJobIDFromPhase(tt.phase); got != tt.want {
			t.Fatalf("currentJobIDFromPhase(%q) = %d, want %d", tt.phase, got, tt.want)
		}
	}
}

func TestEnsureBallastFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ballast.bin")
	if err := ensureBallastFile(path, 8192); err != nil {
		t.Fatalf("ensureBallastFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat ballast: %v", err)
	}
	if info.Size() != 8192 {
		t.Fatalf("ballast size = %d, want 8192", info.Size())
	}
}
