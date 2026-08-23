package sync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceTarballDoesNotExcludeSameNamedRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weft")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, _, err := createSourceTarballWithWorkers(dir, []string{"weft"}, nil, nil, MaxSourceTarballBytes, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	compressed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "go.mod" {
			return
		}
	}
	t.Fatal("go.mod missing from source tarball")
}

func TestSourceTarballHashUsesCanonicalTarBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte(strings.Repeat("canonical source\n", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}

	path, gotHash, err := createSourceTarballWithWorkers(dir, nil, nil, nil, MaxSourceTarballBytes, 2)
	if err != nil {
		t.Fatalf("createSourceTarballWithWorkers: %v", err)
	}
	defer os.Remove(path)

	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	canonicalTar, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read canonical tar: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip reader: %v", err)
	}
	canonicalSum := sha256.Sum256(canonicalTar)
	if want := hex.EncodeToString(canonicalSum[:]); gotHash != want {
		t.Fatalf("source hash = %s, want canonical tar SHA-256 %s", gotHash, want)
	}
	compressedSum := sha256.Sum256(compressed)
	if gotHash == hex.EncodeToString(compressedSum[:]) {
		t.Fatal("source hash unexpectedly identifies the gzip representation")
	}
}

func TestSourceTarballIdentityDoesNotDependOnWorkerCount(t *testing.T) {
	dir := t.TempDir()
	content := []byte(strings.Repeat("parallel deterministic source\n", 100000))
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	onePath, oneHash, err := createSourceTarballWithWorkers(dir, nil, nil, nil, MaxSourceTarballBytes, 1)
	if err != nil {
		t.Fatalf("single-worker tarball: %v", err)
	}
	defer os.Remove(onePath)
	fourPath, fourHash, err := createSourceTarballWithWorkers(dir, nil, nil, nil, MaxSourceTarballBytes, 4)
	if err != nil {
		t.Fatalf("four-worker tarball: %v", err)
	}
	defer os.Remove(fourPath)

	if oneHash != fourHash {
		t.Fatalf("source identity changed with worker count: %s != %s", oneHash, fourHash)
	}
}
