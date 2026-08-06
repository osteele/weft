package sync

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxSourceTarballBytes is the maximum uncompressed source size before
// CreateSourceTarball returns an error. This guards against accidentally
// sweeping up large artifact directories or data files.
const MaxSourceTarballBytes = 500 * 1024 * 1024 // 500 MB

// LargeSourceBlobThresholdBytes is the minimum file size diverted from cloud
// source tarballs into an individually content-addressed object.
const LargeSourceBlobThresholdBytes = 8 * 1024 * 1024 // 8 MiB

// CreateSourceTarball creates a gzip-compressed tarball of localDir, skipping
// entries matching DefaultExcludes(). Returns the path to a temp file and the
// hex-encoded SHA-256 hash of the tarball contents.
//
// Returns an error if the uncompressed source exceeds MaxSourceTarballBytes.
func CreateSourceTarball(localDir string) (tmpPath string, sha256hex string, err error) {
	excludes := sourceExcludes(localDir)
	return createSourceTarballWithOverlays(localDir, excludes, nil)
}

// ErrSourceTooLarge reports that the source tree (plus any staged local:
// input overlays) exceeds MaxSourceTarballBytes. Deterministic: retrying the
// same working tree fails the same way.
var ErrSourceTooLarge = errors.New("source exceeds size limit")

// createSourceTarballWithOverlays is createSourceTarball for staged trees
// that include local: input overlays. overlayInputs (the declared input
// names) selects the size-limit advice: overlaid inputs cannot be excluded
// via .gitignore/.weftignore, so the remedy is the asset store.
func createSourceTarballWithOverlays(localDir string, excludes, overlayInputs []string) (tmpPath string, sha256hex string, err error) {
	return createSourceTarball(localDir, excludes, overlayInputs, nil, MaxSourceTarballBytes)
}

// createSourceTarball writes a deterministic snapshot while omitting exact
// slash-relative paths in omittedPaths. maxBytes <= 0 disables the size cap.
func createSourceTarball(localDir string, excludes, overlayInputs []string, omittedPaths map[string]struct{}, maxBytes int64) (tmpPath string, sha256hex string, err error) {
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
				slog.Debug("skipping unreadable path in source tarball", "component", "sync", "path", path, "error", walkErr)
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
		slashRelPath := filepath.ToSlash(relPath)

		// Check excludes against each path component and the basename
		if shouldExclude(relPath, info, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, omitted := omittedPaths[slashRelPath]; omitted {
			return nil
		}

		// Only include regular files, directories, and symlinks
		isSymlink := info.Mode()&os.ModeSymlink != 0
		if !info.Mode().IsRegular() && !info.IsDir() && !isSymlink {
			return nil
		}

		var linkTarget string
		if isSymlink {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", relPath, err)
			}
		}

		header, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return fmt.Errorf("file info header for %s: %w", relPath, err)
		}
		header.Name = slashRelPath
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

		if info.IsDir() || isSymlink {
			return nil
		}

		totalBytes += info.Size()
		if maxBytes > 0 && totalBytes > maxBytes {
			report := collectSizeReport(localDir, excludes, omittedPaths)
			return fmt.Errorf("%w: source directory exceeds %s limit\n%s\n%s",
				ErrSourceTooLarge, formatSize(maxBytes), report,
				sizeLimitAdvice(overlayInputs, omittedPaths != nil))
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

// collectSizeReport walks localDir (respecting excludes) and returns a
// human-readable report of the largest directories and files.
func collectSizeReport(localDir string, excludes []string, omittedPaths map[string]struct{}) string {
	var files []SnapshotItem
	topLevelBytes := map[string]int64{}
	var totalBytes int64

	_ = filepath.Walk(localDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsPermission(walkErr) {
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return walkErr
		}
		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return nil
		}
		if shouldExclude(relPath, info, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, omitted := omittedPaths[filepath.ToSlash(relPath)]; omitted {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		size := info.Size()
		totalBytes += size
		files = append(files, SnapshotItem{Path: relPath, Bytes: size})
		if top := topLevelDir(relPath); top != "" {
			topLevelBytes[top] += size
		}
		return nil
	})

	const topN = 5
	topDirs := topSnapshotItems(mapToSnapshotItems(topLevelBytes, nil), topN)
	topFiles := topSnapshotItems(files, topN)

	var b strings.Builder
	fmt.Fprintf(&b, "\nTotal non-excluded size: %s\n", formatSize(totalBytes))

	if len(topDirs) > 0 {
		fmt.Fprintf(&b, "\nLargest directories:\n")
		for _, d := range topDirs {
			fmt.Fprintf(&b, "  %-50s %s\n", d.Path+"/", formatSize(d.Bytes))
		}
	}
	if len(topFiles) > 0 {
		fmt.Fprintf(&b, "\nLargest files:\n")
		for _, f := range topFiles {
			fmt.Fprintf(&b, "  %-50s %s\n", f.Path, formatSize(f.Bytes))
		}
	}

	return b.String()
}

// sizeLimitAdvice returns remediation for a residual tarball overflow.
func sizeLimitAdvice(overlayInputs []string, largeFilesDiverted bool) string {
	if !largeFilesDiverted {
		if len(overlayInputs) == 0 {
			return "Add large directories to .gitignore or .weftignore to exclude them."
		}
		return fmt.Sprintf("The staged source includes declared local: inputs (%s), which cannot be excluded via .gitignore/.weftignore. Publish large inputs to the asset store instead: `weft data publish <path> --name <name>`, then reference them with `--input asset:<name>`.",
			strings.Join(overlayInputs, ", "))
	}
	if len(overlayInputs) == 0 {
		return "The residual source tarball is still too large after files of 8 MiB or more were diverted automatically. Add generated or unnecessary directories to .gitignore or .weftignore."
	}
	return fmt.Sprintf("The residual source tarball is still too large after files of 8 MiB or more were diverted automatically. It includes declared local: inputs (%s), which cannot be excluded via .gitignore/.weftignore; split or remove unnecessary input files.",
		strings.Join(overlayInputs, ", "))
}

func formatSize(b int64) string {
	const (
		mb = 1024 * 1024
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%d MB", b/mb)
	default:
		return fmt.Sprintf("%d KB", b/1024)
	}
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

// SourceSizeLimitError builds the ErrSourceTooLarge error for an estimated
// snapshot of totalBytes, with the overlay-aware remediation advice but
// without the per-file size report (pre-claim validation callers don't walk
// twice for the listing; `weft source inspect` provides it on demand).
func SourceSizeLimitError(totalBytes int64, overlayInputs []string) error {
	return fmt.Errorf("%w: source snapshot is %s, over the %s cloud sync limit. %s",
		ErrSourceTooLarge, formatSize(totalBytes), formatSize(MaxSourceTarballBytes),
		sizeLimitAdvice(overlayInputs, true))
}
