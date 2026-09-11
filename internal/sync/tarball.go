package sync

import (
	"archive/tar"
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

	"github.com/klauspost/pgzip"
)

// MaxSourceTarballBytes is the maximum uncompressed source size before
// CreateSourceTarball returns an error. This guards against accidentally
// sweeping up large artifact directories or data files.
const MaxSourceTarballBytes = 500 * 1024 * 1024 // 500 MB

// LargeSourceBlobThresholdBytes is the minimum file size diverted from cloud
// source tarballs into an individually content-addressed object.
const LargeSourceBlobThresholdBytes = 8 * 1024 * 1024 // 8 MiB

const sourceGzipBlockSize = 1 << 20

// CreateSourceTarball creates a gzip-compressed tarball of localDir, skipping
// entries matching DefaultExcludes(). Returns the path to a temp file and the
// hex-encoded SHA-256 hash of the canonical uncompressed tar stream. The hash
// is independent of the gzip encoder and its worker count.
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
	return createSourceTarballWithWorkers(localDir, excludes, overlayInputs, omittedPaths, maxBytes, sourceWorkerLimit())
}

func createSourceTarballWithWorkers(localDir string, excludes, overlayInputs []string, omittedPaths map[string]struct{}, maxBytes int64, workers int) (tmpPath string, sha256hex string, err error) {
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

	if workers < 1 {
		workers = 1
	}
	gw := pgzip.NewWriter(tmpFile)
	if err := gw.SetConcurrency(sourceGzipBlockSize, workers); err != nil {
		return "", "", fmt.Errorf("configure gzip concurrency: %w", err)
	}
	hasher := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(gw, hasher))

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
			if !symlinkTargetWithinRoot(slashRelPath, filepath.ToSlash(linkTarget)) {
				// Out-of-root link (absolute target, or a relative one
				// leaving the tree): snapshot the target's content instead
				// of the link. The extractor rejects escaping links, and a
				// preserved link would dangle on the remote host anyway.
				externalInfo, err := os.Stat(path)
				if err != nil {
					return fmt.Errorf("symlink %s -> %s: %w (out-of-tree symlink targets must exist to be snapshotted)", relPath, linkTarget, err)
				}
				return snapshotExternalTarget(tw, path, slashRelPath, externalInfo, map[string]struct{}{}, 1, &totalBytes, maxBytes, excludes)
			}
		}

		header, err := newCanonicalTarHeader(slashRelPath, info, linkTarget)
		if err != nil {
			return fmt.Errorf("file info header for %s: %w", relPath, err)
		}

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

// newCanonicalTarHeader builds a tar header with name set and timestamps and
// ownership zeroed, so identical source content produces identical canonical
// tar streams regardless of when files were modified.
func newCanonicalTarHeader(name string, info os.FileInfo, linkTarget string) (*tar.Header, error) {
	header, err := tar.FileInfoHeader(info, linkTarget)
	if err != nil {
		return nil, err
	}
	header.Name = name
	header.ModTime = time.Time{}
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""
	return header, nil
}

// maxExternalSnapshotDepth bounds how deep snapshotExternalTarget follows the
// content behind out-of-root symlinks. The visited-realpath set catches true
// cycles; this cap backstops pathological link chains.
const maxExternalSnapshotDepth = 32

// snapshotExternalTarget writes the real content behind an out-of-root symlink
// into tw as plain entries rooted at the link's archive name, keeping the
// archive self-contained: an absolute link preserved verbatim would dangle on
// the remote host, and the extractor rejects links escaping the extraction
// root. With tw nil it only measures the bytes the entries would occupy, for
// source size estimation.
//
// Symlinks encountered inside the external tree are followed rather than
// preserved, so nothing behind an external link can trip extraction-time
// symlink validation. Child entries are checked against excludes by their
// archive-relative name, matching the main walk: an excluded directory skips
// its whole subtree. totalBytes accumulates regular-file bytes; maxBytes > 0
// enforces the same size cap as the main walk. active holds visited real
// directories for cycle detection; depth is the link-follow budget.
func snapshotExternalTarget(tw *tar.Writer, path, destName string, info os.FileInfo, active map[string]struct{}, depth int, totalBytes *int64, maxBytes int64, excludes []string) error {
	switch {
	case info.Mode().IsRegular():
		if tw == nil {
			*totalBytes += info.Size()
			return nil
		}
		return writeExternalFileEntry(tw, path, destName, info, totalBytes, maxBytes)
	case info.IsDir():
		if depth > maxExternalSnapshotDepth {
			return fmt.Errorf("symlink chain under %s exceeds %d levels; refusing to snapshot a probable cycle", path, maxExternalSnapshotDepth)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", path, err)
		}
		if _, cyclic := active[resolved]; cyclic {
			return fmt.Errorf("symlink cycle through %s", resolved)
		}
		active[resolved] = struct{}{}
		defer delete(active, resolved)
		if tw != nil {
			header, err := newCanonicalTarHeader(destName, info, "")
			if err != nil {
				return fmt.Errorf("file info header for %s: %w", destName, err)
			}
			if err := tw.WriteHeader(header); err != nil {
				return fmt.Errorf("write header for %s: %w", destName, err)
			}
		}
		// ReadDir returns entries sorted by name, keeping the archive
		// deterministic.
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("read directory %s: %w", path, err)
		}
		for _, entry := range entries {
			childDest := destName + "/" + entry.Name()
			childPath := filepath.Join(path, entry.Name())
			// Stat follows links: everything behind the external tree is
			// flattened, so no escaping link can reappear deeper down.
			childInfo, err := os.Stat(childPath)
			if err != nil {
				// Vendored trees can carry stale links; skip rather than fail
				// the whole snapshot, but say so in the log.
				slog.Warn("skipping unreadable entry behind external symlink", "component", "sync", "path", childPath, "error", err)
				continue
			}
			if shouldExclude(childDest, childInfo, excludes) {
				slog.Debug("skipping excluded entry behind external symlink", "component", "sync", "path", childPath)
				continue
			}
			if err := snapshotExternalTarget(tw, childPath, childDest, childInfo, active, depth+1, totalBytes, maxBytes, excludes); err != nil {
				return err
			}
		}
		return nil
	default:
		slog.Debug("skipping non-regular entry behind external symlink", "component", "sync", "path", path, "mode", info.Mode())
		return nil
	}
}

// writeExternalFileEntry writes one regular file's header and content into tw.
func writeExternalFileEntry(tw *tar.Writer, path, name string, info os.FileInfo, totalBytes *int64, maxBytes int64) error {
	header, err := newCanonicalTarHeader(name, info, "")
	if err != nil {
		return fmt.Errorf("file info header for %s: %w", name, err)
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header for %s: %w", name, err)
	}
	*totalBytes += info.Size()
	if maxBytes > 0 && *totalBytes > maxBytes {
		return fmt.Errorf("%w: content behind external symlink at %s exceeds %s limit", ErrSourceTooLarge, name, formatSize(maxBytes))
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copy %s: %w", name, err)
	}
	return nil
}

// externalSymlinkBytes measures the bytes the snapshot carries behind an
// out-of-root symlink, mirroring snapshotExternalTarget with tw nil. destName
// is the link's slash-relative path in the snapshot, so child entries are
// exclusion-checked by their archive-relative names.
func externalSymlinkBytes(path, destName string, excludes []string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	var total int64
	if err := snapshotExternalTarget(nil, path, destName, info, map[string]struct{}{}, 1, &total, 0, excludes); err != nil {
		return 0, err
	}
	return total, nil
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
	// Exclude patterns describe entries inside the source root. Applying a
	// basename pattern to the root itself can erase the entire snapshot when a
	// project ignores a same-named build product (for example, repo `weft` with
	// a `.gitignore` entry `weft`).
	if relPath == "." {
		return false
	}
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
