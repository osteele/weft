package artifacts

import (
	"path/filepath"
	"testing"
)

func TestLocalArtifactsDirHonorsXDGDataHome(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	got, err := LocalArtifactsDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataHome, "weft", "artifacts"); got != want {
		t.Fatalf("LocalArtifactsDir() = %q, want %q", got, want)
	}
}
