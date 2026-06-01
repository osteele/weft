package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPerJobSourceMarkerFile_NamingConvention(t *testing.T) {
	got := PerJobSourceMarkerFile(2301)
	want := ".weft-source.2301.sha256"
	if got != want {
		t.Errorf("PerJobSourceMarkerFile(2301) = %q, want %q", got, want)
	}
}

func TestReadSourceMarkerForJob_PrefersPerJobFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SourceMarkerFile), []byte("rolling-sha\n"), 0644); err != nil {
		t.Fatalf("write rolling marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, PerJobSourceMarkerFile(42)), []byte("per-job-sha\n"), 0644); err != nil {
		t.Fatalf("write per-job marker: %v", err)
	}

	got, err := ReadSourceMarkerForJob(dir, 42)
	if err != nil {
		t.Fatalf("ReadSourceMarkerForJob: %v", err)
	}
	if got != "per-job-sha" {
		t.Errorf("got %q, want per-job-sha (per-job file should win over rolling)", got)
	}
}

func TestReadSourceMarkerForJob_FallsBackToRolling(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SourceMarkerFile), []byte("rolling-sha\n"), 0644); err != nil {
		t.Fatalf("write rolling marker: %v", err)
	}

	got, err := ReadSourceMarkerForJob(dir, 42)
	if err != nil {
		t.Fatalf("ReadSourceMarkerForJob: %v", err)
	}
	if got != "rolling-sha" {
		t.Errorf("got %q, want rolling-sha (rolling marker should win when per-job absent)", got)
	}
}

func TestReadSourceMarkerForJob_BothAbsent(t *testing.T) {
	dir := t.TempDir()

	_, err := ReadSourceMarkerForJob(dir, 42)
	if err == nil {
		t.Fatal("expected error when both per-job and rolling markers are absent")
	}
}
