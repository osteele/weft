package sync

import (
	"context"
	"fmt"
	"os"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

// UploadSourceToR2 creates a content-addressed tarball of localDir and uploads
// it to R2 if it doesn't already exist. Returns the R2 key.
func UploadSourceToR2(ctx context.Context, r2Client *r2.Client, localDir string) (string, error) {
	tmpPath, hash, err := CreateSourceTarball(localDir)
	if err != nil {
		return "", fmt.Errorf("create source tarball: %w", err)
	}
	defer os.Remove(tmpPath)

	key := dataplane.SourceTarball(hash)

	exists, err := r2Client.ObjectExists(ctx, key)
	if err != nil {
		return "", fmt.Errorf("check source exists: %w", err)
	}
	if exists {
		return key, nil
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return "", fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()

	if err := r2Client.PutObject(ctx, key, f, "application/gzip"); err != nil {
		return "", fmt.Errorf("upload source tarball: %w", err)
	}

	return key, nil
}
