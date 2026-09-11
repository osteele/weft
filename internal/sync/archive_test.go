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

// TestTarballDereferencesOutOfRootSymlink is the wb122 regression test: trees
// that vendor out-of-tree content as absolute symlinks (e.g. ~/.claude/skills)
// could never run remotely — the extractor rejects escaping links, so every
// job failed with pinned_source_fetch_failed after the full queue wait.
// Creation now snapshots the link target's content as plain entries.
func TestTarballDereferencesOutOfRootSymlink(t *testing.T) {
	external := t.TempDir()
	if err := os.MkdirAll(filepath.Join(external, "skill-a"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "skill-a", "SKILL.md"), []byte("skill a body"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "top.txt"), []byte("external top"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("skill-a", filepath.Join(external, "inner-rel")); err != nil {
		t.Fatal(err)
	}
	// Excluded names behind the link must not ship: the dereference applies
	// the same source excludes as in-tree entries.
	if err := os.WriteFile(filepath.Join(external, ".env"), []byte("SECRET=1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(external, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, ".git", "HEAD"), []byte("ref"), 0644); err != nil {
		t.Fatal(err)
	}

	srcDir := t.TempDir()
	// Absolute dir link: the ~/.claude/skills shape.
	if err := os.Symlink(external, filepath.Join(srcDir, "skills")); err != nil {
		t.Fatal(err)
	}
	// Absolute file link in a subdirectory.
	if err := os.MkdirAll(filepath.Join(srcDir, "vendor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "top.txt"), filepath.Join(srcDir, "vendor", "top.txt")); err != nil {
		t.Fatal(err)
	}
	// Relative link that leaves the tree.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "rel.txt"), []byte("relative"), 0644); err != nil {
		t.Fatal(err)
	}
	relTarget, err := filepath.Rel(srcDir, filepath.Join(outside, "rel.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relTarget, filepath.Join(srcDir, "rel-link")); err != nil {
		t.Fatal(err)
	}
	// In-tree links are still preserved as links, even pointing at a
	// dereferenced out-of-root snapshot.
	if err := os.Symlink("skills", filepath.Join(srcDir, "alias")); err != nil {
		t.Fatal(err)
	}

	tarPath, hash, err := CreateSourceTarball(srcDir)
	if err != nil {
		t.Fatalf("createSourceTarball: %v", err)
	}
	defer os.Remove(tarPath)

	destDir := t.TempDir()
	if err := ExtractTarball(tarPath, destDir); err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}

	// The out-of-root links became real content.
	if info, err := os.Lstat(filepath.Join(destDir, "skills")); err != nil {
		t.Fatalf("Lstat(skills): %v", err)
	} else if !info.IsDir() {
		t.Fatalf("skills is %v, want a real directory", info.Mode())
	}
	assertFileContent(t, filepath.Join(destDir, "skills", "skill-a", "SKILL.md"), "skill a body")
	assertFileContent(t, filepath.Join(destDir, "vendor", "top.txt"), "external top")
	assertFileContent(t, filepath.Join(destDir, "rel-link"), "relative")
	// Links inside external content are flattened too, so nothing in the
	// archive can trip extraction-time symlink validation.
	assertFileContent(t, filepath.Join(destDir, "skills", "inner-rel", "SKILL.md"), "skill a body")
	for _, excluded := range []string{".env", filepath.Join(".git", "HEAD")} {
		if fileExists(filepath.Join(destDir, "skills", excluded)) {
			t.Errorf("excluded path %s was snapshotted behind the external symlink", excluded)
		}
	}
	if got, err := os.Readlink(filepath.Join(destDir, "alias")); err != nil || got != "skills" {
		t.Fatalf("Readlink(alias) = %q, %v; want %q", got, err, "skills")
	}

	// Snapshotting is deterministic.
	tarPath2, hash2, err := CreateSourceTarball(srcDir)
	if err != nil {
		t.Fatalf("createSourceTarball again: %v", err)
	}
	defer os.Remove(tarPath2)
	if hash != hash2 {
		t.Errorf("hash changed across identical snapshots: %s vs %s", hash, hash2)
	}
}

// TestTarballRejectsDanglingOutOfRootSymlink: a broken out-of-tree link cannot
// be snapshotted. Failing at submission with a clear error beats failing
// extraction after the upload and queue wait.
func TestTarballRejectsDanglingOutOfRootSymlink(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(srcDir, "gone")); err != nil {
		t.Fatal(err)
	}
	_, _, err := createSourceTarballWithOverlays(srcDir, nil, nil)
	if err == nil {
		t.Fatal("createSourceTarball accepted a dangling out-of-root symlink")
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("error %q does not name the broken link", err)
	}
}

// TestTarballRejectsExternalSymlinkCycle: out-of-root links whose content
// cycles back through the tree must fail creation instead of recursing
// forever.
func TestTarballRejectsExternalSymlinkCycle(t *testing.T) {
	external := t.TempDir()
	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(external, "x"), 0755); err != nil {
		t.Fatal(err)
	}
	// src/a -> external/x; external/x/b -> src/c; src/c -> external.
	if err := os.Symlink(filepath.Join(external, "x"), filepath.Join(srcDir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(srcDir, "c"), filepath.Join(external, "x", "b")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(srcDir, "c")); err != nil {
		t.Fatal(err)
	}
	_, _, err := createSourceTarballWithOverlays(srcDir, nil, nil)
	if err == nil {
		t.Fatal("createSourceTarball accepted an external symlink cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error %q does not report a cycle", err)
	}
}

// TestIncludedSourceBytesCountsExternalSymlinkContent keeps SizeBytes (disk
// estimation) consistent with what the tarball carries behind out-of-root
// links.
func TestIncludedSourceBytesCountsExternalSymlinkContent(t *testing.T) {
	external := t.TempDir()
	payload := bytes.Repeat([]byte("x"), 4096)
	if err := os.WriteFile(filepath.Join(external, "big.txt"), payload, 0644); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	if err := os.Symlink(external, filepath.Join(srcDir, "link")); err != nil {
		t.Fatal(err)
	}
	total, err := IncludedSourceBytes(srcDir)
	if err != nil {
		t.Fatalf("IncludedSourceBytes: %v", err)
	}
	if total != int64(len(payload)) {
		t.Errorf("IncludedSourceBytes = %d, want %d", total, len(payload))
	}
}

// assertFileContent fails the test unless path is a regular file with exactly
// the given content (in particular, not a symlink).
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is still a symlink", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", path, data, want)
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
