package artifacts

import (
	"os"
	"testing"
)

func TestCaptureAndReadSystemBlob(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	stored, size, digest, err := CaptureSystemBlob("telemetry", []byte("one\ntwo\n"))
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	got, err := ReadSystemBlob(stored, size, digest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "one\ntwo\n" {
		t.Fatalf("bytes = %q", got)
	}
	path, err := LocalPathFromStored(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSystemBlob(stored, size, digest); err == nil {
		t.Fatal("tampered blob read succeeded")
	}
}

func TestReadSystemBlobRejectsTraversal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := ReadSystemBlob("../secret", 1, "00"); err == nil {
		t.Fatal("traversal path accepted")
	}
}
