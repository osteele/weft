package logcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAndRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	content := "line1\nline2\nline3\n"
	if err := Write(42, content); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(42)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != content {
		t.Fatalf("Read returned %q, want %q", got, content)
	}
}

func TestWriteCreatesMetaFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := Write(100, "data"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Write marks as complete by default
	if !IsComplete(100) {
		t.Fatal("expected IsComplete=true after Write")
	}

	// Meta file should exist
	if _, err := os.Stat(metaPath(100)); err != nil {
		t.Fatalf("meta file not found: %v", err)
	}
}

func TestWriteWithMetaPartial(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := WriteWithMeta(200, "partial data", false); err != nil {
		t.Fatalf("WriteWithMeta: %v", err)
	}

	if IsComplete(200) {
		t.Fatal("expected IsComplete=false for partial cache")
	}

	got, err := Read(200)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "partial data" {
		t.Fatalf("Read returned %q, want %q", got, "partial data")
	}
}

func TestWriteForRunStoresRunIDMetadata(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := WriteForRun(201, 77, "run data"); err != nil {
		t.Fatalf("WriteForRun: %v", err)
	}

	runID, ok := RunID(201)
	if !ok {
		t.Fatal("expected RunID metadata to be present")
	}
	if runID != 77 {
		t.Fatalf("RunID = %d, want 77", runID)
	}
}

func TestRunIDMissingWhenMetaDoesNotIncludeRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := WriteWithMeta(202, "legacy cache", true); err != nil {
		t.Fatalf("WriteWithMeta: %v", err)
	}

	if _, ok := RunID(202); ok {
		t.Fatal("expected no RunID for legacy cache metadata")
	}
}

func TestIsCompleteReturnsFalseForMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if IsComplete(999) {
		t.Fatal("expected IsComplete=false for non-existent job")
	}
}

func TestDeleteRemovesMetaFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := Write(300, "data"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := Delete(300); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if Exists(300) {
		t.Fatal("expected log file to be removed")
	}
	if _, err := os.Stat(metaPath(300)); !os.IsNotExist(err) {
		t.Fatal("expected meta file to be removed")
	}
}

func TestDeleteNonExistentIsNoOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := Delete(999); err != nil {
		t.Fatalf("Delete non-existent: %v", err)
	}
}

func TestPruneRemovesOldMetaFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := Write(400, "old data"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Backdate the log file
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(CachePath(400), oldTime, oldTime)

	pruned, err := Prune(24 * time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned, got %d", pruned)
	}

	if Exists(400) {
		t.Fatal("expected log file to be pruned")
	}
	if _, err := os.Stat(metaPath(400)); !os.IsNotExist(err) {
		t.Fatal("expected meta file to be pruned")
	}
}

func TestPruneKeepsRecentFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := Write(500, "recent data"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	pruned, err := Prune(24 * time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("expected 0 pruned, got %d", pruned)
	}

	if !Exists(500) {
		t.Fatal("expected recent log file to be kept")
	}
}

func TestExistsAndCachePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if Exists(600) {
		t.Fatal("expected Exists=false before write")
	}

	if err := Write(600, "data"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !Exists(600) {
		t.Fatal("expected Exists=true after write")
	}

	path := CachePath(600)
	if filepath.Base(path) != "600.log" {
		t.Fatalf("unexpected cache path: %s", path)
	}
}
