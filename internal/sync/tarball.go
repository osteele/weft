package sync

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxSourceTarballBytes is the maximum uncompressed source size before
// CreateSourceTarball returns an error. This guards against accidentally
// sweeping up large artifact directories or data files.
const MaxSourceTarballBytes = 500 * 1024 * 1024 // 500 MB

// CreateSourceTarball creates a gzip-compressed tarball of localDir, skipping
// entries matching DefaultExcludes(). Returns the path to a temp file and the
// hex-encoded SHA-256 hash of the tarball contents.
//
// Returns an error if the uncompressed source exceeds MaxSourceTarballBytes.
func CreateSourceTarball(localDir string) (tmpPath string, sha256hex string, err error) {
	excludes := sourceExcludes(localDir)

	tmpFile, err := os.CreateTemp("", "weft-source-*.tar.gz")
	if err != nil {
		return "", "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath = tmpFile.Name()
	defer func() {
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	mw := io.MultiWriter(tmpFile, hasher)

	gw := gzip.NewWriter(mw)
	tw := tar.NewWriter(gw)

	localDir = strings.TrimRight(localDir, "/")

	var totalBytes int64
	err = filepath.Walk(localDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsPermission(walkErr) {
				log.Printf("source tarball: skipping unreadable path %s: %v", path, walkErr)
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return walkErr
		}

		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}

		// Check excludes against each path component and the basename
		if shouldExclude(relPath, info, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Only include regular files and directories
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("file info header for %s: %w", relPath, err)
		}
		header.Name = relPath
		// Zero out timestamps/uid/gid for deterministic hashing —
		// identical source content produces the same tarball hash
		// regardless of when files were modified.
		header.ModTime = time.Time{}
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write header for %s: %w", relPath, err)
		}

		if info.IsDir() {
			return nil
		}

		totalBytes += info.Size()
		if totalBytes > MaxSourceTarballBytes {
			return fmt.Errorf("source directory exceeds %d MB limit (at %s); check for large data files or build artifacts not in DefaultExcludes",
				MaxSourceTarballBytes/(1024*1024), relPath)
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relPath, err)
		}
		defer f.Close()

		if _, err := io.Copy(tw, f); err != nil {
			return fmt.Errorf("copy %s: %w", relPath, err)
		}

		return nil
	})
	if err != nil {
		return "", "", fmt.Errorf("walk %s: %w", localDir, err)
	}

	if err := tw.Close(); err != nil {
		return "", "", fmt.Errorf("close tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", "", fmt.Errorf("close gzip writer: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", "", fmt.Errorf("close temp file: %w", err)
	}

	return tmpPath, hex.EncodeToString(hasher.Sum(nil)), nil
}

// shouldExclude returns true if the given path should be excluded from the tarball.
func shouldExclude(relPath string, info os.FileInfo, excludes []string) bool {
	name := info.Name()
	for _, pattern := range excludes {
		// Match against basename
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
		// Match against each path component
		for _, component := range strings.Split(relPath, string(filepath.Separator)) {
			if matched, _ := filepath.Match(pattern, component); matched {
				return true
			}
		}
	}
	return false
}
