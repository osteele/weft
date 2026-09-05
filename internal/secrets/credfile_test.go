package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

// A credential file's mode is invisible unless something looks: the value reads
// back fine and nothing fails. These tests pin that weft looks.
func TestPermissiveCredentialFileIsStillReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("SLACK_WEBHOOK=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCredentialFile(path)
	if err != nil {
		t.Fatalf("a permissive file must still be read, not refused: %v", err)
	}
	if string(got) != "SLACK_WEBHOOK=x\n" {
		t.Errorf("content = %q", got)
	}
}

func TestOwnerOnlyCredentialFileReadsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("SLACK_WEBHOOK=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialFile(path); err != nil {
		t.Fatalf("owner-only file should read without error: %v", err)
	}
}

// Absence is the common case — these files are optional fallbacks for an
// environment variable — so it must surface as the underlying error for the
// caller to interpret, not be swallowed into an empty read.
func TestMissingCredentialFileReturnsItsError(t *testing.T) {
	_, err := ReadCredentialFile(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("a missing credential file must return its error, not empty content")
	}
	if !os.IsNotExist(err) {
		t.Errorf("error should be a not-exist error the caller can test: %v", err)
	}
}
