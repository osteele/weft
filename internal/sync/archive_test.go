package sync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTarballRoundTripPreservesSymlink is a regression test: source tarballs
// used to silently drop symlinks, so projects with a symlinked data/ worked
// on-prem (rsync ships links) but failed with FileNotFoundError on rentals.
func TestTarballRoundTripPreservesSymlink(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "realdata"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "realdata", "f.txt"), []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realdata", filepath.Join(srcDir, "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realdata/f.txt", filepath.Join(srcDir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	tarPath, _, err := createSourceTarballWithOverlays(srcDir, nil, nil)
	if err != nil {
		t.Fatalf("createSourceTarball: %v", err)
	}
	defer os.Remove(tarPath)

	destDir := t.TempDir()
	if err := ExtractTarball(tarPath, destDir); err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}

	for link, wantTarget := range map[string]string{
		"data":     "realdata",
		"link.txt": "realdata/f.txt",
	} {
		got, err := os.Readlink(filepath.Join(destDir, link))
		if err != nil {
			t.Fatalf("Readlink(%s): %v", link, err)
		}
		if got != wantTarget {
			t.Errorf("symlink %s target = %q, want %q", link, got, wantTarget)
		}
	}

	// The symlinked dir resolves to the real content after extraction.
	data, err := os.ReadFile(filepath.Join(destDir, "data", "f.txt"))
	if err != nil {
		t.Fatalf("read through extracted symlink: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("content through symlink = %q, want %q", data, "payload")
	}
}

// TestProvenanceHashChangesWithSymlinkTarget is a regression test: provenance
// hashing was symlink-blind, so trees differing only in a link target hashed
// identically and stale-source preflight checks passed incorrectly.
func TestProvenanceHashChangesWithSymlinkTarget(t *testing.T) {
	makeTree := func(target string) string {
		dir := t.TempDir()
		for _, sub := range []string{"a", "b"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(target, filepath.Join(dir, "data")); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	hashA, err := ComputeSourceSHA256(makeTree("a"))
	if err != nil {
		t.Fatalf("ComputeSourceSHA256(a): %v", err)
	}
	hashB, err := ComputeSourceSHA256(makeTree("b"))
	if err != nil {
		t.Fatalf("ComputeSourceSHA256(b): %v", err)
	}
	if hashA == hashB {
		t.Errorf("hashes identical (%s) despite differing symlink targets", hashA)
	}

	hashA2, err := ComputeSourceSHA256(makeTree("a"))
	if err != nil {
		t.Fatalf("ComputeSourceSHA256(a) again: %v", err)
	}
	if hashA != hashA2 {
		t.Errorf("hash not deterministic: %s vs %s", hashA, hashA2)
	}
}

// makeTarGz builds an in-memory gzip-compressed tarball from explicit headers.
func makeTarGz(t *testing.T, entries []tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for i := range entries {
		hdr := entries[i]
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write header %q: %v", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(hdr.Size))); err != nil {
				t.Fatalf("write body %q: %v", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// TestExtractRejectsTarSlip is a regression test: extraction had no tar-slip
// guard, so entries with ../ or absolute names could write outside destDir.
func TestExtractRejectsTarSlip(t *testing.T) {
	cases := []struct {
		name  string
		entry tar.Header
	}{
		{"dotdot escape", tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}},
		{"nested dotdot escape", tar.Header{Name: "ok/../../evil.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}},
		{"absolute path", tar.Header{Name: "/tmp/evil.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			destDir := t.TempDir()
			buf := makeTarGz(t, []tar.Header{tc.entry})
			err := ExtractTarballReader(buf, destDir)
			if err == nil {
				t.Fatalf("ExtractTarballReader accepted escaping entry %q", tc.entry.Name)
			}
			if escaped := filepath.Join(destDir, "..", "evil.txt"); fileExists(escaped) {
				t.Errorf("escaping entry was written to %s", escaped)
			}
		})
	}
}

// TestExtractRejectsEscapingSymlink: symlink entries whose target is absolute
// or resolves outside the extraction root are rejected.
func TestExtractRejectsEscapingSymlink(t *testing.T) {
	cases := []struct {
		name     string
		linkName string
		target   string
	}{
		{"absolute target", "link", "/etc/passwd"},
		{"dotdot target", "link", "../outside"},
		{"nested dotdot target", "sub/link", "../../outside"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			destDir := t.TempDir()
			buf := makeTarGz(t, []tar.Header{
				{Name: tc.linkName, Typeflag: tar.TypeSymlink, Linkname: tc.target, Mode: 0777},
			})
			err := ExtractTarballReader(buf, destDir)
			if err == nil {
				t.Fatalf("ExtractTarballReader accepted symlink %q -> %q", tc.linkName, tc.target)
			}
			if !strings.Contains(err.Error(), "symlink") {
				t.Errorf("error %q does not mention symlink", err)
			}
		})
	}
}

// TestExtractAllowsInTreeSymlink: symlinks that stay within the extraction
// root extract successfully, including ones using a relative ../ that still
// resolves inside the root.
func TestExtractAllowsInTreeSymlink(t *testing.T) {
	destDir := t.TempDir()
	buf := makeTarGz(t, []tar.Header{
		{Name: "realdata", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "sub", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "sub/link", Typeflag: tar.TypeSymlink, Linkname: "../realdata", Mode: 0777},
	})
	if err := ExtractTarballReader(buf, destDir); err != nil {
		t.Fatalf("ExtractTarballReader: %v", err)
	}
	got, err := os.Readlink(filepath.Join(destDir, "sub", "link"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != "../realdata" {
		t.Errorf("symlink target = %q, want %q", got, "../realdata")
	}
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
