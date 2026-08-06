package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

type sourceObjectStore interface {
	ObjectExists(context.Context, string) (bool, error)
	PutObject(context.Context, string, io.Reader, string) error
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
	result, err := uploadSourceRootsToR2(ctx, r2Client, localDir, inputs, nil, onProgress, false)
	if err != nil {
		return "", err
	}
	if len(result.Manifest.Roots) == 0 {
		return "", fmt.Errorf("source manifest for %s has no roots", localDir)
	}
	return result.Manifest.Roots[0].R2Key, nil
}

// UploadCloudSourceRootsToR2ForInputs uploads cloud source roots, diverting
// large files into content-addressed blob objects.
func UploadCloudSourceRootsToR2ForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string) (SourceUploadResult, error) {
	return uploadSourceRootsToR2(ctx, r2Client, localDir, inputs, nil, nil, true)
}

// UploadSourceRootsToR2WithProgressForInputs uploads each content-addressed
// source root tarball and returns the ordered manifest for the whole source
// state.
func UploadSourceRootsToR2WithProgressForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, onProgress UploadSourceProgressFunc) (SourceUploadResult, error) {
	return UploadSourceRootsToR2WithProgressForInputsAndCommands(ctx, r2Client, localDir, inputs, nil, onProgress)
}

// UploadSourceRootsToR2WithProgressForInputsAndCommands uploads source roots
// including project/script-derived uv path sources from commands.
func UploadSourceRootsToR2WithProgressForInputsAndCommands(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, commands []string, onProgress UploadSourceProgressFunc) (SourceUploadResult, error) {
	return uploadSourceRootsToR2(ctx, r2Client, localDir, inputs, commands, onProgress, true)
}

func uploadSourceRootsToR2(ctx context.Context, store sourceObjectStore, localDir string, inputs []string, commands []string, onProgress UploadSourceProgressFunc, divertLargeFiles bool) (SourceUploadResult, error) {
	if onProgress == nil {
		onProgress = func(string) {}
	}

	onProgress("hashing source")
	build, err := buildSourceManifestForInputsAndCommands(localDir, inputs, commands, divertLargeFiles)
	if err != nil {
		return SourceUploadResult{}, fmt.Errorf("create source manifest: %w", err)
	}
	defer build.cleanup()
	defer removeFiles(build.tarPaths)
	manifest := build.manifest

	onProgress("checking cache")
	for i, root := range manifest.Roots {
		for j, blob := range root.Blobs {
			exists, err := store.ObjectExists(ctx, blob.R2Key)
			if err != nil {
				return SourceUploadResult{}, fmt.Errorf("check source blob %s exists: %w", blob.RelPath, err)
			}
			if exists {
				continue
			}
			f, cleanup, err := openSourceBlobSnapshot(build.blobPaths[i][j], blob.SHA256)
			if err != nil {
				return SourceUploadResult{}, fmt.Errorf("open source blob %s: %w", blob.RelPath, err)
			}
			onProgress("uploading source blob")
			if err := store.PutObject(ctx, blob.R2Key, f, "application/octet-stream"); err != nil {
				cleanup()
				return SourceUploadResult{}, fmt.Errorf("upload source blob %s: %w", blob.RelPath, err)
			}
			if err := f.Close(); err != nil {
				cleanup()
				return SourceUploadResult{}, fmt.Errorf("close source blob %s: %w", blob.RelPath, err)
			}
			cleanup()
		}

		exists, err := store.ObjectExists(ctx, root.R2Key)
		if err != nil {
			return SourceUploadResult{}, fmt.Errorf("check source root %s exists: %w", root.LocalPath, err)
		}
		if exists {
			continue
		}
		f, err := os.Open(build.tarPaths[i])
		if err != nil {
			return SourceUploadResult{}, fmt.Errorf("open tarball: %w", err)
		}
		onProgress("uploading source")
		if err := store.PutObject(ctx, root.R2Key, f, "application/gzip"); err != nil {
			f.Close()
			return SourceUploadResult{}, fmt.Errorf("upload source root %s: %w", root.LocalPath, err)
		}
		if err := f.Close(); err != nil {
			return SourceUploadResult{}, fmt.Errorf("close tarball %s: %w", build.tarPaths[i], err)
		}
	}

	onProgress("ready")
	return SourceUploadResult{Manifest: manifest}, nil
}

func openSourceBlobSnapshot(filename, expectedHash string) (*os.File, func(), error) {
	src, err := os.Open(filename)
	if err != nil {
		return nil, nil, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp("", "weft-source-blob-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		cleanup()
		return nil, nil, err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("source changed while snapshotting: SHA-256 is %s, want %s", actualHash, expectedHash)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		cleanup()
		return nil, nil, err
	}
	return tmp, cleanup, nil
}

func stageSourceDirWithLocalInputs(localDir string, inputs []string) (string, []string, func(), error) {
	return stageSourceDirWithLocalInputsLimit(localDir, inputs, MaxSourceTarballBytes)
}

func stageSourceDirWithLocalInputsLimit(localDir string, inputs []string, maxBytes int64) (string, []string, func(), error) {
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
	dir, cleanup, err := buildSourceSnapshotWithOverlaysLimit(localDir, overlays, maxBytes)
	return dir, names, cleanup, err
}
