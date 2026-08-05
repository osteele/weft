package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/osteele/weft/internal/r2"
)

// UploadSourceProgressFunc receives coarse source upload phase updates.
// Phases are descriptive labels such as "hashing source", "checking cache",
// "uploading source", and "ready".
type UploadSourceProgressFunc func(phase string)

// SourceUploadResult describes the uploaded multi-root source state.
type SourceUploadResult struct {
	Manifest SourceManifest
}

// UploadSourceToR2 creates a content-addressed tarball of localDir and uploads
// it to R2 if it doesn't already exist. Returns the R2 key.
func UploadSourceToR2(ctx context.Context, r2Client *r2.Client, localDir string) (string, error) {
	return UploadSourceToR2ForInputs(ctx, r2Client, localDir, nil)
}

// UploadSourceToR2ForInputs creates a content-addressed tarball for localDir,
// ensuring declared local: inputs are included even if the source snapshot would
// normally exclude them.
func UploadSourceToR2ForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string) (string, error) {
	return UploadSourceToR2WithProgressForInputs(ctx, r2Client, localDir, inputs, nil)
}

// UploadSourceToR2WithProgress creates a content-addressed tarball of localDir,
// uploads it to R2 if needed, and reports coarse phase changes via onProgress.
func UploadSourceToR2WithProgress(ctx context.Context, r2Client *r2.Client, localDir string, onProgress UploadSourceProgressFunc) (string, error) {
	return UploadSourceToR2WithProgressForInputs(ctx, r2Client, localDir, nil, onProgress)
}

// UploadSourceToR2WithProgressForInputs creates a content-addressed tarball of
// localDir, overlays declared local: inputs when needed, uploads it to R2 if
// needed, and reports coarse phase changes via onProgress.
func UploadSourceToR2WithProgressForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, onProgress UploadSourceProgressFunc) (string, error) {
	result, err := UploadSourceRootsToR2WithProgressForInputs(ctx, r2Client, localDir, inputs, onProgress)
	if err != nil {
		return "", err
	}
	if len(result.Manifest.Roots) == 0 {
		return "", fmt.Errorf("source manifest for %s has no roots", localDir)
	}
	return result.Manifest.Roots[0].R2Key, nil
}

// UploadSourceRootsToR2WithProgressForInputs uploads each content-addressed
// source root tarball and returns the ordered manifest for the whole source
// state.
func UploadSourceRootsToR2WithProgressForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, onProgress UploadSourceProgressFunc) (SourceUploadResult, error) {
	if onProgress == nil {
		onProgress = func(string) {}
	}

	onProgress("hashing source")
	manifest, tmpPaths, err := BuildSourceManifestForInputs(localDir, inputs)
	if err != nil {
		return SourceUploadResult{}, fmt.Errorf("create source manifest: %w", err)
	}
	defer removeFiles(tmpPaths)

	onProgress("checking cache")
	for i, root := range manifest.Roots {
		exists, err := r2Client.ObjectExists(ctx, root.R2Key)
		if err != nil {
			return SourceUploadResult{}, fmt.Errorf("check source root %s exists: %w", root.LocalPath, err)
		}
		if exists {
			continue
		}
		f, err := os.Open(tmpPaths[i])
		if err != nil {
			return SourceUploadResult{}, fmt.Errorf("open tarball: %w", err)
		}
		onProgress("uploading source")
		if err := r2Client.PutObject(ctx, root.R2Key, f, "application/gzip"); err != nil {
			f.Close()
			return SourceUploadResult{}, fmt.Errorf("upload source root %s: %w", root.LocalPath, err)
		}
		if err := f.Close(); err != nil {
			return SourceUploadResult{}, fmt.Errorf("close tarball %s: %w", tmpPaths[i], err)
		}
	}

	onProgress("ready")
	return SourceUploadResult{Manifest: manifest}, nil
}

func stageSourceDirWithLocalInputs(localDir string, inputs []string) (string, []string, func(), error) {
	localDir, err := filepath.Abs(localDir)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve source directory: %w", err)
	}
	overlays, err := LocalInputOverlays(localDir, inputs, RequireLocalInput)
	if err != nil {
		return "", nil, nil, err
	}
	if len(overlays) == 0 {
		return "", nil, func() {}, nil
	}
	names := make([]string, len(overlays))
	for i, o := range overlays {
		names[i] = o.Input
	}
	dir, cleanup, err := buildSourceSnapshotWithOverlays(localDir, overlays)
	return dir, names, cleanup, err
}
