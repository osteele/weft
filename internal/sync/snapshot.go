package sync

import (
	"fmt"
	"os"
	"path/filepath"
)

// BuildSourceSnapshot materializes a source snapshot for localDir using the
// default source excludes, then overlays declared local: inputs into that
// snapshot. The returned directory must be cleaned up by the caller.
func BuildSourceSnapshot(localDir string, inputs []string) (string, func(), error) {
	localDir, err := filepath.Abs(localDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve source directory: %w", err)
	}

	overlays, err := localInputOverlays(localDir, inputs)
	if err != nil {
		return "", nil, err
	}
	return buildSourceSnapshotWithOverlays(localDir, overlays)
}

func buildSourceSnapshotWithOverlays(localDir string, overlays []localOverlay) (string, func(), error) {
	stageDir, err := os.MkdirTemp("", "weft-source-stage-*")
	if err != nil {
		return "", nil, fmt.Errorf("create staged source dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stageDir) }

	mainTarball, _, err := CreateSourceTarball(localDir)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage source snapshot: %w", err)
	}
	defer os.Remove(mainTarball)
	if err := ExtractTarball(mainTarball, stageDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("extract staged source snapshot: %w", err)
	}

	for _, overlay := range overlays {
		target := filepath.Join(stageDir, overlay.rel)
		if err := CopyPath(overlay.abs, target); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("stage declared local input %q: %w", overlay.input, err)
		}
	}
	return stageDir, cleanup, nil
}
