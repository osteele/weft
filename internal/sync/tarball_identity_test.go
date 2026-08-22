package sync

import (
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
