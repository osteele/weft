package processguard

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestSnapshotsDifferDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weft")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old binary: %v", err)
	}
	old := CapturePath(path)
	if !old.Valid {
		t.Fatal("old snapshot invalid")
	}
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("new binary"), 0o755); err != nil {
		t.Fatalf("write new binary: %v", err)
	}
	newSnap := CapturePath(path)
	if !SnapshotsDiffer(old, newSnap) {
		t.Fatalf("SnapshotsDiffer returned false for replaced binary: old=%+v new=%+v", old, newSnap)
	}
}

func TestActiveRunnerBinaryChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weft")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old binary: %v", err)
	}
	old := CapturePath(path)
	if !old.Valid {
		t.Fatal("old snapshot invalid")
	}
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("new binary"), 0o755); err != nil {
		t.Fatalf("write new binary: %v", err)
	}
	changed, err := ActiveRunnerBinaryChanged(db.BinaryIdentity{
		Path:        old.Path,
		Size:        old.Size,
		ModTimeUnix: old.ModTimeUnix,
		Dev:         old.Dev,
		Ino:         old.Ino,
	})
	if err != nil {
		t.Fatalf("ActiveRunnerBinaryChanged: %v", err)
	}
	if !changed {
		t.Fatal("ActiveRunnerBinaryChanged = false, want true")
	}
}
