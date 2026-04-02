package sync

import (
	"context"
	"fmt"
	"os"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

// UploadSourceProgressFunc receives coarse source upload phase updates.
// Phases are descriptive labels such as "hashing source", "checking cache",
// "uploading source", and "ready".
type UploadSourceProgressFunc func(phase string)

// UploadSourceToR2 creates a content-addressed tarball of localDir and uploads
// it to R2 if it doesn't already exist. Returns the R2 key.
func UploadSourceToR2(ctx context.Context, r2Client *r2.Client, localDir string) (string, error) {
	return UploadSourceToR2WithProgress(ctx, r2Client, localDir, nil)
}

// UploadSourceToR2WithProgress creates a content-addressed tarball of localDir,
// uploads it to R2 if needed, and reports coarse phase changes via onProgress.
func UploadSourceToR2WithProgress(ctx context.Context, r2Client *r2.Client, localDir string, onProgress UploadSourceProgressFunc) (string, error) {
	if onProgress == nil {
		onProgress = func(string) {}
	}
	onProgress("hashing source")

	tmpPath, hash, err := CreateSourceTarball(localDir)
	if err != nil {
		return "", fmt.Errorf("create source tarball: %w", err)
	}
	defer os.Remove(tmpPath)

	key := dataplane.SourceTarball(hash)

	onProgress("checking cache")
	exists, err := r2Client.ObjectExists(ctx, key)
	if err != nil {
		return "", fmt.Errorf("check source exists: %w", err)
	}
	if exists {
		onProgress("ready")
		return key, nil
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return "", fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()

	onProgress("uploading source")
	if err := r2Client.PutObject(ctx, key, f, "application/gzip"); err != nil {
		return "", fmt.Errorf("upload source tarball: %w", err)
	}

	onProgress("ready")
	return key, nil
}
