package sync

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SnapshotItem records the size of an included file or aggregated directory.
type SnapshotItem struct {
	Path                  string `json:"path"`
	Bytes                 int64  `json:"bytes"`
	ApproxCompressedBytes int64  `json:"approx_compressed_bytes,omitempty"`
}

// SnapshotInspection summarizes the local source snapshot after exclude rules.
type SnapshotInspection struct {
	LocalDir        string         `json:"local_dir"`
	Excludes        []string       `json:"excludes"`
	FileCount       int            `json:"file_count"`
	DirectoryCount  int            `json:"directory_count"`
	TotalBytes      int64          `json:"total_bytes"`
	CompressedBytes *int64         `json:"compressed_bytes,omitempty"`
	LimitBytes      int64          `json:"limit_bytes"`
	OverLimit       bool           `json:"over_limit"`
	LargestFiles    []SnapshotItem `json:"largest_files"`
	LargestTopLevel []SnapshotItem `json:"largest_top_level_dirs"`
	TarballHash     string         `json:"tarball_hash,omitempty"`
}

// InspectSnapshot measures the local source snapshot using the same exclude
// rules as source sync and campaign tarballs.
func InspectSnapshot(localDir string, topFiles, topDirs int) (*SnapshotInspection, error) {
	resolvedDir, err := filepath.Abs(localDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", localDir, err)
	}

	excludes := sourceExcludes(resolvedDir)
	fileItems := []SnapshotItem{}
	topLevelBytes := map[string]int64{}
	topLevelApproxCompressed := map[string]int64{}
	result := &SnapshotInspection{
		LocalDir:   resolvedDir,
		Excludes:   excludes,
		LimitBytes: MaxSourceTarballBytes,
	}

	err = filepath.Walk(resolvedDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relPath, err := filepath.Rel(resolvedDir, path)
		if err != nil {
			return err
		}

		if shouldExclude(relPath, info, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}

		if info.IsDir() {
			if relPath != "." {
				result.DirectoryCount++
			}
			return nil
		}

		size := info.Size()
		result.FileCount++
		result.TotalBytes += size
		fileItems = append(fileItems, SnapshotItem{Path: relPath, Bytes: size})

		if top := topLevelDir(relPath); top != "" {
			topLevelBytes[top] += size
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", resolvedDir, err)
	}

	approxCompressedByPath, err := estimateCompressedContributions(resolvedDir, excludes)
	if err != nil {
		return nil, err
	}
	for i := range fileItems {
		fileItems[i].ApproxCompressedBytes = approxCompressedByPath[fileItems[i].Path]
		if top := topLevelDir(fileItems[i].Path); top != "" {
			topLevelApproxCompressed[top] += fileItems[i].ApproxCompressedBytes
		}
	}

	result.OverLimit = result.TotalBytes > result.LimitBytes
	result.LargestFiles = topSnapshotItems(fileItems, topFiles)
	result.LargestTopLevel = topSnapshotItems(mapToSnapshotItems(topLevelBytes, topLevelApproxCompressed), topDirs)

	if !result.OverLimit {
		tmpPath, hash, err := CreateSourceTarball(resolvedDir)
		if err != nil {
			return nil, fmt.Errorf("create source tarball: %w", err)
		}
		defer os.Remove(tmpPath)

		info, err := os.Stat(tmpPath)
		if err != nil {
			return nil, fmt.Errorf("stat tarball %s: %w", tmpPath, err)
		}
		size := info.Size()
		result.CompressedBytes = &size
		result.TarballHash = hash
	}

	return result, nil
}

func estimateCompressedContributions(localDir string, excludes []string) (map[string]int64, error) {
	counter := &countingWriter{w: io.Discard}
	gw := gzip.NewWriter(counter)
	tw := tar.NewWriter(gw)
	contributions := map[string]int64{}

	err := filepath.Walk(localDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}

		if shouldExclude(relPath, info, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("file info header for %s: %w", relPath, err)
		}
		header.Name = relPath
		header.ModTime = time.Time{}
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""

		before := counter.n
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write header for %s: %w", relPath, err)
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open %s: %w", relPath, err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return fmt.Errorf("copy %s: %w", relPath, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", relPath, err)
			}
		}
		if err := gw.Flush(); err != nil {
			return fmt.Errorf("flush gzip writer: %w", err)
		}
		if info.Mode().IsRegular() {
			contributions[relPath] = counter.n - before
		}
		return nil
	})
	if err != nil {
		tw.Close()
		gw.Close()
		return nil, fmt.Errorf("estimate compressed contributions: %w", err)
	}
	if err := tw.Close(); err != nil {
		gw.Close()
		return nil, fmt.Errorf("close tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("close gzip writer: %w", err)
	}
	return contributions, nil
}

func topLevelDir(relPath string) string {
	dir := filepath.Dir(relPath)
	if dir == "." {
		return ""
	}
	for dir != "." {
		parent := filepath.Dir(dir)
		if parent == "." {
			return dir
		}
		dir = parent
	}
	return ""
}

func mapToSnapshotItems(sizes map[string]int64, approxCompressed map[string]int64) []SnapshotItem {
	items := make([]SnapshotItem, 0, len(sizes))
	for path, bytes := range sizes {
		items = append(items, SnapshotItem{
			Path:                  path,
			Bytes:                 bytes,
			ApproxCompressedBytes: approxCompressed[path],
		})
	}
	return items
}

func topSnapshotItems(items []SnapshotItem, limit int) []SnapshotItem {
	sort.Slice(items, func(i, j int) bool {
		if items[i].ApproxCompressedBytes != items[j].ApproxCompressedBytes {
			return items[i].ApproxCompressedBytes > items[j].ApproxCompressedBytes
		}
		if items[i].Bytes != items[j].Bytes {
			return items[i].Bytes > items[j].Bytes
		}
		return items[i].Path < items[j].Path
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}
