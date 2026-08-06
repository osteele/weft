package sync

import (
	"fmt"
	"os"
	"path/filepath"
)

// SnapshotResult holds the output of BuildSourceSnapshot.
type SnapshotResult struct {
	Dir      string   // staged snapshot directory
	Cleanup  func()   // must be called to remove the staging directory
	Overlays []string // declared local: inputs that were overlaid (e.g. "local:data/conllu/")
}

// BuildSourceSnapshot materializes a source snapshot for localDir using the
// default source excludes, then overlays declared local: inputs into that
// snapshot. The returned directory must be cleaned up by the caller.
func BuildSourceSnapshot(localDir string, inputs []string) (SnapshotResult, error) {
	localDir, err := filepath.Abs(localDir)
	if err != nil {
		return SnapshotResult{}, fmt.Errorf("resolve source directory: %w", err)
	}

	overlays, err := LocalInputOverlays(localDir, inputs, RequireLocalInput)
	if err != nil {
		return SnapshotResult{}, err
	}

	var overlayNames []string
	for _, o := range overlays {
		overlayNames = append(overlayNames, o.Input)
	}

	dir, cleanup, err := buildSourceSnapshotWithOverlays(localDir, overlays)
	if err != nil {
		return SnapshotResult{}, err
	}
	return SnapshotResult{Dir: dir, Cleanup: cleanup, Overlays: overlayNames}, nil
}

func buildSourceSnapshotWithOverlays(localDir string, overlays []LocalOverlay) (string, func(), error) {
	return buildSourceSnapshotWithOverlaysLimit(localDir, overlays, MaxSourceTarballBytes)
}

func buildSourceSnapshotWithOverlaysLimit(localDir string, overlays []LocalOverlay, maxBytes int64) (string, func(), error) {
	stageDir, err := os.MkdirTemp("", "weft-source-stage-*")
	if err != nil {
		return "", nil, fmt.Errorf("create staged source dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stageDir) }

	mainTarball, _, err := createSourceTarball(localDir, sourceExcludes(localDir), nil, nil, maxBytes)
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
		target := filepath.Join(stageDir, overlay.Rel)
		if err := CopyPath(overlay.Abs, target); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("stage declared local input %q: %w", overlay.Input, err)
		}
	}
	return stageDir, cleanup, nil
}
