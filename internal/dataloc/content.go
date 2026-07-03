package dataloc

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ContentInfo describes a local file or directory asset.
type ContentInfo struct {
	Hash        string
	SizeBytes   int64
	ContentType ContentType
}

type manifestEntry struct {
	relPath string
	size    int64
	hash    string
}

// DigestPath computes a stable SHA256 and logical byte size for a file or
// directory. Directory size is the sum of regular file sizes.
func DigestPath(path string) (ContentInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ContentInfo{}, err
	}
	if !info.IsDir() {
		hash, size, err := digestFile(path)
		if err != nil {
			return ContentInfo{}, err
		}
		return ContentInfo{Hash: hash, SizeBytes: size, ContentType: ContentTypeFile}, nil
	}
	entries, err := directoryManifest(path)
	if err != nil {
		return ContentInfo{}, err
	}
	h := sha256.New()
	var total int64
	for _, entry := range entries {
		total += entry.size
		fmt.Fprintf(h, "%s\t%d\t%s\n", entry.relPath, entry.size, entry.hash)
	}
	return ContentInfo{Hash: fmt.Sprintf("%x", h.Sum(nil)), SizeBytes: total, ContentType: ContentTypeDirectory}, nil
}

// WriteDirectoryArchive writes a deterministic tar.gz archive of dir to out.
func WriteDirectoryArchive(dir string, out io.Writer) error {
	entries, err := directoryManifest(dir)
	if err != nil {
		return err
	}
	gz, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Name = ""
	gz.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		fullPath := filepath.Join(dir, filepath.FromSlash(entry.relPath))
		if err := writeTarFile(tw, fullPath, entry); err != nil {
			tw.Close()
			gz.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}

func writeTarFile(tw *tar.Writer, fullPath string, entry manifestEntry) error {
	header := &tar.Header{
		Name:     entry.relPath,
		Mode:     0o644,
		Size:     entry.size,
		ModTime:  time.Unix(0, 0),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	return nil
}

func directoryManifest(root string) ([]manifestEntry, error) {
	var entries []manifestEntry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("directory asset %s contains non-regular file %s", root, path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("directory asset %s has invalid relative path %s", root, rel)
		}
		hash, size, err := digestFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, manifestEntry{relPath: rel, size: size, hash: hash})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})
	return entries, nil
}

func digestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), size, nil
}
