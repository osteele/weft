package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
)

func TestParsePruneTimeFilter_MutuallyExclusive(t *testing.T) {
	_, err := parsePruneTimeFilter("7d", "2026-01-01", time.Now())
	if err == nil {
		t.Fatal("expected error when both --older-than and --since are set")
	}
}

func TestParseOlderThanDuration(t *testing.T) {
	d, err := parseOlderThanDuration("7")
	if err != nil {
		t.Fatalf("parse older-than integer days: %v", err)
	}
	if d != 7*24*time.Hour {
		t.Fatalf("duration = %s, want %s", d, 7*24*time.Hour)
	}

	d, err = parseOlderThanDuration("36h")
	if err != nil {
		t.Fatalf("parse older-than duration: %v", err)
	}
	if d != 36*time.Hour {
		t.Fatalf("duration = %s, want %s", d, 36*time.Hour)
	}
}

func TestBuildLocalPrunePlan_SelectsOnlyRestorableFiles(t *testing.T) {
	database := db.SetupTestDB(t)
	scopeRoot := t.TempDir()

	outputDir := filepath.Join(scopeRoot, "output")
	dataDir := filepath.Join(scopeRoot, "data")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "skip.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write skip.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "artifact.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write artifact.json: %v", err)
	}

	jobID, err := db.RecordQueued(database, "host-a", scopeRoot, "echo hi", "desc")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host: "host-a",
		Asset: dataloc.DataAsset{
			Kind: dataloc.AssetJobOutput,
			ID:   strconv.FormatInt(jobID, 10) + "/output/keep.txt",
		},
		Path:      "output/keep.txt",
		SizeBytes: 1,
		LastSeen:  time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	if err := db.UpsertArtifact(database, db.Artifact{
		JobID:      jobID,
		Path:       "data/artifact.json",
		StoredPath: filepath.Join(strconv.FormatInt(jobID, 10), "data/artifact.json"),
		SizeBytes:  2,
	}); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	plan, err := buildLocalPrunePlan(database, scopeRoot, true, true, pruneTimeFilter{})
	if err != nil {
		t.Fatalf("buildLocalPrunePlan: %v", err)
	}

	got := map[string]bool{}
	for _, f := range plan.Files {
		got[f.RelPath] = true
	}
	if !got["output/keep.txt"] {
		t.Fatalf("expected output/keep.txt in prune plan: %+v", plan.Files)
	}
	if !got["data/artifact.json"] {
		t.Fatalf("expected data/artifact.json in prune plan: %+v", plan.Files)
	}
	if got["output/skip.txt"] {
		t.Fatalf("did not expect output/skip.txt in prune plan: %+v", plan.Files)
	}
}

func TestBuildLocalPrunePlan_OlderThanFilter(t *testing.T) {
	database := db.SetupTestDB(t)
	scopeRoot := t.TempDir()
	outputDir := filepath.Join(scopeRoot, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	oldFile := filepath.Join(outputDir, "old.txt")
	newFile := filepath.Join(outputDir, "new.txt")
	if err := os.WriteFile(oldFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(newFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}
	now := time.Now()
	if err := os.Chtimes(oldFile, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}
	if err := os.Chtimes(newFile, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("chtimes new: %v", err)
	}

	jobID, err := db.RecordQueued(database, "host-a", scopeRoot, "echo hi", "desc")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host: "host-a",
		Asset: dataloc.DataAsset{
			Kind: dataloc.AssetJobOutput,
			ID:   strconv.FormatInt(jobID, 10) + "/output/old.txt",
		},
		Path:      "output/old.txt",
		SizeBytes: 1,
		LastSeen:  time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset old: %v", err)
	}
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host: "host-a",
		Asset: dataloc.DataAsset{
			Kind: dataloc.AssetJobOutput,
			ID:   strconv.FormatInt(jobID, 10) + "/output/new.txt",
		},
		Path:      "output/new.txt",
		SizeBytes: 1,
		LastSeen:  time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset new: %v", err)
	}
	// keep artifact list tied to the same job to avoid fallback behavior differences
	if err := db.UpsertArtifact(database, db.Artifact{
		JobID:      jobID,
		Path:       "output/old.txt",
		StoredPath: filepath.Join(strconv.FormatInt(jobID, 10), "output/old.txt"),
		SizeBytes:  1,
	}); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	plan, err := buildLocalPrunePlan(database, scopeRoot, true, false, pruneTimeFilter{olderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("buildLocalPrunePlan: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0].RelPath != "output/old.txt" {
		t.Fatalf("unexpected filtered files: %+v", plan.Files)
	}
}
