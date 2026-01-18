package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveArtifactOutputPath(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "artifact.txt")
	if err := os.WriteFile(source, []byte("data"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	dest, err := resolveArtifactOutputPath(source, "")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if dest != "artifact.txt" {
		t.Fatalf("expected default basename, got %q", dest)
	}

	dest, err = resolveArtifactOutputPath(source, "-")
	if err != nil {
		t.Fatalf("resolve stdout: %v", err)
	}
	if dest != "-" {
		t.Fatalf("expected '-', got %q", dest)
	}

	dest, err = resolveArtifactOutputPath(source, tmp)
	if err != nil {
		t.Fatalf("resolve dir: %v", err)
	}
	if dest != filepath.Join(tmp, "artifact.txt") {
		t.Fatalf("expected dir join, got %q", dest)
	}

	dest, err = resolveArtifactOutputPath(source, filepath.Join(tmp, "out.txt"))
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	if dest != filepath.Join(tmp, "out.txt") {
		t.Fatalf("expected explicit file, got %q", dest)
	}
}

func TestCopyToWriter(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "artifact.txt")
	want := []byte("artifact contents\n")
	if err := os.WriteFile(source, want, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var buf bytes.Buffer
	if err := copyToWriter(source, &buf); err != nil {
		t.Fatalf("copy to writer: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("unexpected content: %q", buf.Bytes())
	}
}
