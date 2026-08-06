package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/controlplane"
)

const sourceBlobCopyTimeout = 20 * time.Minute

type cachedSourceBlob struct {
	blob      controlplane.SourceBlob
	hash      string
	cachePath string
}

func sourceBlobCachePath(hash string) string {
	return filepath.Join(sourceCacheDir(), "blobs", strings.ToLower(hash))
}

func ensureSourceBlobsCached(bucket string, blobs []controlplane.SourceBlob) error {
	validated, err := validateSourceBlobs(blobs)
	if err != nil {
		return err
	}
	missingByHash := make(map[string]cachedSourceBlob)
	var missing []cachedSourceBlob
	for _, item := range validated {
		matches, err := fileMatchesSHA256(item.cachePath, item.hash)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("verify cached blob %s for %s: %w", item.blob.R2Key, item.blob.RelPath, err)
		}
		if matches {
			continue
		}
		if _, exists := missingByHash[item.hash]; !exists {
			missingByHash[item.hash] = item
			missing = append(missing, item)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	cacheDir := filepath.Join(sourceCacheDir(), "blobs")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("create source blob cache: %w", err)
	}
	downloadDir, err := os.MkdirTemp(cacheDir, ".download-")
	if err != nil {
		return fmt.Errorf("create blob download directory: %w", err)
	}
	defer os.RemoveAll(downloadDir)

	listPath := filepath.Join(downloadDir, "files-from.txt")
	keys := make([]string, 0, len(missing))
	for _, item := range missing {
		keys = append(keys, item.blob.R2Key)
	}
	if err := os.WriteFile(listPath, []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write blob batch list: %w", err)
	}
	if err := runSourceBlobBatchCopy(bucket, listPath, downloadDir); err != nil {
		for _, item := range missing {
			downloadPath := filepath.Join(downloadDir, filepath.FromSlash(item.blob.R2Key))
			if err := os.MkdirAll(filepath.Dir(downloadPath), 0o755); err != nil {
				return fmt.Errorf("prepare individual blob %s (%s): %w", item.blob.R2Key, item.blob.RelPath, err)
			}
			if err := runSourceBlobCopyTo(bucket, item.blob.R2Key, downloadPath); err != nil {
				return fmt.Errorf("download blob %s for %s after batch failure: %w", item.blob.R2Key, item.blob.RelPath, err)
			}
		}
	}

	for _, item := range missing {
		downloadPath := filepath.Join(downloadDir, filepath.FromSlash(item.blob.R2Key))
		matches, err := fileMatchesSHA256(downloadPath, item.hash)
		if err != nil {
			return fmt.Errorf("verify downloaded blob %s for %s: %w", item.blob.R2Key, item.blob.RelPath, err)
		}
		if !matches {
			actual, err := fileSHA256(downloadPath)
			if err != nil {
				return fmt.Errorf("hash downloaded blob %s for %s: %w", item.blob.R2Key, item.blob.RelPath, err)
			}
			return fmt.Errorf("downloaded blob %s for %s has SHA-256 %s, want %s", item.blob.R2Key, item.blob.RelPath, actual, item.hash)
		}
		if err := os.Rename(downloadPath, item.cachePath); err != nil {
			return fmt.Errorf("store blob %s in cache: %w", item.blob.R2Key, err)
		}
	}
	return nil
}

func validateSourceBlobs(blobs []controlplane.SourceBlob) ([]cachedSourceBlob, error) {
	validated := make([]cachedSourceBlob, 0, len(blobs))
	paths := make(map[string]struct{}, len(blobs))
	for i, blob := range blobs {
		if strings.TrimSpace(blob.R2Key) == "" {
			return nil, fmt.Errorf("source blob %d missing r2_key", i+1)
		}
		if !safeSlashRelativePath(blob.R2Key) || strings.ContainsAny(blob.R2Key, "\r\n") {
			return nil, fmt.Errorf("source blob %d has unsafe r2_key %q", i+1, blob.R2Key)
		}
		if !safeSlashRelativePath(blob.RelPath) {
			return nil, fmt.Errorf("source blob %d has unsafe rel_path %q", i+1, blob.RelPath)
		}
		cleanRelPath := path.Clean(blob.RelPath)
		if _, exists := paths[cleanRelPath]; exists {
			return nil, fmt.Errorf("source blob %d duplicates rel_path %q", i+1, cleanRelPath)
		}
		paths[cleanRelPath] = struct{}{}
		hash, err := normalizeSHA256(blob.SHA256)
		if err != nil {
			return nil, fmt.Errorf("source blob %d (%s): %w", i+1, blob.RelPath, err)
		}
		blob.RelPath = cleanRelPath
		validated = append(validated, cachedSourceBlob{
			blob:      blob,
			hash:      hash,
			cachePath: sourceBlobCachePath(hash),
		})
	}
	return validated, nil
}

func safeSlashRelativePath(value string) bool {
	clean := path.Clean(value)
	return value != "" && clean != "." && !path.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, "../")
}

func normalizeSHA256(value string) (string, error) {
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("invalid SHA-256 %q", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("invalid SHA-256 %q: %w", value, err)
	}
	return strings.ToLower(value), nil
}

func runSourceBlobBatchCopy(bucket, listPath, downloadDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sourceBlobCopyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "copy", fmt.Sprintf("r2:%s", bucket), downloadDir, "--files-from", listPath)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runSourceBlobCopyTo(bucket, r2Key, downloadPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sourceBlobCopyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "copyto", fmt.Sprintf("r2:%s/%s", bucket, r2Key), downloadPath)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func fileMatchesSHA256(filename, expected string) (bool, error) {
	actual, err := fileSHA256(filename)
	if err != nil {
		return false, err
	}
	return actual == expected, nil
}

func fileSHA256(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func materializeSourceBlobs(remoteDir string, blobs []controlplane.SourceBlob) error {
	validated, err := validateSourceBlobs(blobs)
	if err != nil {
		return err
	}
	for _, item := range validated {
		targetPath, err := sourceBlobTarget(remoteDir, item.blob.RelPath)
		if err != nil {
			return err
		}
		if err := copySourceBlob(item.cachePath, targetPath, item.hash); err != nil {
			return fmt.Errorf("place blob %s at %s: %w", item.blob.R2Key, item.blob.RelPath, err)
		}
	}
	return nil
}

func sourceBlobTarget(root, relPath string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootAbs, filepath.FromSlash(relPath))
	if target == rootAbs || !strings.HasPrefix(target, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("source blob path escapes remote dir: %q", relPath)
	}
	return target, nil
}

func copySourceBlob(cachePath, targetPath, expectedHash string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	src, err := os.Open(cachePath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".weft-blob-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), src)
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectedHash {
		return fmt.Errorf("cached content has SHA-256 %s, want %s", actual, expectedHash)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return err
	}
	return nil
}
