package sync

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ExtractTarball extracts a gzip-compressed tarball into destDir.
func ExtractTarball(tarballPath, destDir string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()
	return ExtractTarballReader(f, destDir)
}

// ExtractTarballReader extracts a gzip-compressed tar stream into destDir.
func ExtractTarballReader(r io.Reader, destDir string) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		target, err := safeExtractTarget(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("create directory %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("create parent directory for %s: %w", target, err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("create file %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("extract %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %s: %w", target, err)
			}
		case tar.TypeSymlink:
			if err := checkSymlinkWithinRoot(hdr.Name, hdr.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("create parent directory for %s: %w", target, err)
			}
			// Remove any existing entry so re-extraction over a previous
			// extraction doesn't fail with EEXIST.
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("replace %s: %w", target, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("create symlink %s: %w", target, err)
			}
		}
	}
}

// safeExtractTarget validates a tar entry name against tar-slip attacks and
// returns the cleaned destination path. Entries with absolute paths or whose
// destination escapes destDir via ".." are rejected.
func safeExtractTarget(destDir, name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("tar entry %q escapes extraction root", name)
	}
	return filepath.Join(destDir, name), nil
}

// checkSymlinkWithinRoot rejects symlink entries whose link target is absolute
// or resolves outside the extraction root. Tarballs are self-produced, so this
// is hardening against corrupt or mis-built archives rather than a security
// boundary.
func checkSymlinkWithinRoot(name, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("tar symlink %q has absolute target %q", name, linkname)
	}
	if !filepath.IsLocal(filepath.Join(filepath.Dir(name), linkname)) {
		return fmt.Errorf("tar symlink %q target %q escapes extraction root", name, linkname)
	}
	return nil
}
