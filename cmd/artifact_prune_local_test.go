package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
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

	plan, err := buildLocalPrunePlan(database, scopeRoot, true, true, pruneTimeFilter{}, nil, nil)
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

	plan, err := buildLocalPrunePlan(database, scopeRoot, true, false, pruneTimeFilter{olderThan: 24 * time.Hour}, nil, nil)
	if err != nil {
		t.Fatalf("buildLocalPrunePlan: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0].RelPath != "output/old.txt" {
		t.Fatalf("unexpected filtered files: %+v", plan.Files)
	}
}

// writeRestorableOutput creates a restorable output/ file under scopeRoot and
// records the backing job + asset so it qualifies for prune-local.
func writeRestorableOutput(t *testing.T, database *sql.DB, scopeRoot, name string, size int) {
	t.Helper()
	outputDir := filepath.Join(scopeRoot, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, name), bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	jobID, err := db.RecordQueued(database, "host-a", scopeRoot, "echo hi", "desc")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host: "host-a",
		Asset: dataloc.DataAsset{
			Kind: dataloc.AssetJobOutput,
			ID:   strconv.FormatInt(jobID, 10) + "/output/" + name,
		},
		Path:      "output/" + name,
		SizeBytes: int64(size),
		LastSeen:  time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
}

func TestPruneOneScope_ReturnsEffectiveTotals(t *testing.T) {
	database := db.SetupTestDB(t)
	prevOutputs, prevArtifacts := pruneOutputs, pruneArtifacts
	pruneOutputs, pruneArtifacts = true, true
	t.Cleanup(func() { pruneOutputs, pruneArtifacts = prevOutputs, prevArtifacts })

	scopeA := t.TempDir()
	scopeB := t.TempDir()
	writeRestorableOutput(t, database, scopeA, "a.bin", 10)
	writeRestorableOutput(t, database, scopeB, "b1.bin", 30)
	writeRestorableOutput(t, database, scopeB, "b2.bin", 5)

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	resA, err := pruneOneScope(cmd, database, scopeA, pruneTimeFilter{}, true, nil, nil)
	if err != nil {
		t.Fatalf("pruneOneScope A: %v", err)
	}
	if resA.Files != 1 || resA.Bytes != 10 {
		t.Fatalf("scope A result = %+v, want Files=1 Bytes=10", resA)
	}
	resB, err := pruneOneScope(cmd, database, scopeB, pruneTimeFilter{}, true, nil, nil)
	if err != nil {
		t.Fatalf("pruneOneScope B: %v", err)
	}
	if resB.Files != 2 || resB.Bytes != 35 {
		t.Fatalf("scope B result = %+v, want Files=2 Bytes=35", resB)
	}

	if got := resA.Files + resB.Files; got != 3 {
		t.Fatalf("rollup files = %d, want 3", got)
	}
	if got := resA.Bytes + resB.Bytes; got != 45 {
		t.Fatalf("rollup bytes = %d, want 45", got)
	}
}

func TestPrintPruneRollup(t *testing.T) {
	dryRun := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(dryRun)
	printPruneRollup(cmd, 3, 12, 2048, true)
	out := dryRun.String()
	if !strings.Contains(out, "== Total (3 projects) ==") {
		t.Fatalf("dry-run rollup missing header: %q", out)
	}
	if !strings.Contains(out, "Would free: 2.0 KiB (2,048 bytes) from 12 file(s) across 3 projects") {
		t.Fatalf("dry-run rollup line wrong: %q", out)
	}

	apply := &bytes.Buffer{}
	cmd.SetOut(apply)
	printPruneRollup(cmd, 2, 7, 2048, false)
	out = apply.String()
	if !strings.Contains(out, "== Total (2 projects) ==") {
		t.Fatalf("apply rollup missing header: %q", out)
	}
	if !strings.Contains(out, "Deleted: 7 file(s), reclaimed 2.0 KiB (2,048 bytes) across 2 projects") {
		t.Fatalf("apply rollup line wrong: %q", out)
	}
}

func TestFindProjectSubdirs(t *testing.T) {
	database := db.SetupTestDB(t)
	parent := t.TempDir()
	projectA := filepath.Join(parent, "alpha")
	projectB := filepath.Join(parent, "beta")
	deepProject := filepath.Join(parent, "gamma", "nested")
	for _, d := range []string{projectA, projectB, deepProject} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Each subdir has a job whose working_dir is that subdir.
	if _, err := db.RecordQueued(database, "host-a", projectA, "echo a", "a"); err != nil {
		t.Fatalf("RecordQueued alpha: %v", err)
	}
	if _, err := db.RecordQueued(database, "host-a", projectB, "echo b", "b"); err != nil {
		t.Fatalf("RecordQueued beta: %v", err)
	}
	// gamma/nested also has a job; its first segment under parent is "gamma".
	if _, err := db.RecordQueued(database, "host-a", deepProject, "echo g", "g"); err != nil {
		t.Fatalf("RecordQueued gamma/nested: %v", err)
	}

	jobs, err := db.ListJobsByStatuses(database, nil, "", "", 0, nil, "")
	if err != nil {
		t.Fatalf("ListJobsByStatuses: %v", err)
	}
	subdirs, scopeIsProject := findProjectSubdirs(jobs, parent)
	if scopeIsProject {
		t.Fatalf("parent should not be a project; jobs only in subdirs")
	}
	want := []string{
		filepath.Join(parent, "alpha"),
		filepath.Join(parent, "beta"),
		filepath.Join(parent, "gamma"),
	}
	if len(subdirs) != len(want) {
		t.Fatalf("subdirs = %v, want %v", subdirs, want)
	}
	for i, w := range want {
		if subdirs[i] != w {
			t.Fatalf("subdirs[%d] = %q, want %q", i, subdirs[i], w)
		}
	}
}

func TestFindProjectSubdirs_ScopeIsProject(t *testing.T) {
	database := db.SetupTestDB(t)
	scope := t.TempDir()
	child := filepath.Join(scope, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	if _, err := db.RecordQueued(database, "host-a", scope, "echo s", "s"); err != nil {
		t.Fatalf("RecordQueued scope: %v", err)
	}
	if _, err := db.RecordQueued(database, "host-a", child, "echo c", "c"); err != nil {
		t.Fatalf("RecordQueued child: %v", err)
	}

	jobs, err := db.ListJobsByStatuses(database, nil, "", "", 0, nil, "")
	if err != nil {
		t.Fatalf("ListJobsByStatuses: %v", err)
	}
	subdirs, scopeIsProject := findProjectSubdirs(jobs, scope)
	if !scopeIsProject {
		t.Fatalf("scope should be detected as project")
	}
	want := []string{filepath.Join(scope, "child")}
	if len(subdirs) != 1 || subdirs[0] != want[0] {
		t.Fatalf("subdirs = %v, want %v", subdirs, want)
	}
}

func TestResolvePruneScopes_AutoRecurses(t *testing.T) {
	database := db.SetupTestDB(t)
	parent := t.TempDir()
	a := filepath.Join(parent, "alpha")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := db.RecordQueued(database, "host-a", a, "echo a", "a"); err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	jobs, err := db.ListJobsByStatuses(database, nil, "", "", 0, nil, "")
	if err != nil {
		t.Fatalf("ListJobsByStatuses: %v", err)
	}
	scopes, err := resolvePruneScopes(jobs, parent, "auto")
	if err != nil {
		t.Fatalf("resolvePruneScopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != a {
		t.Fatalf("scopes = %v, want [%s]", scopes, a)
	}
}

func TestResolvePruneScopes_OffStaysSingle(t *testing.T) {
	database := db.SetupTestDB(t)
	parent := t.TempDir()
	a := filepath.Join(parent, "alpha")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := db.RecordQueued(database, "host-a", a, "echo a", "a"); err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	jobs, err := db.ListJobsByStatuses(database, nil, "", "", 0, nil, "")
	if err != nil {
		t.Fatalf("ListJobsByStatuses: %v", err)
	}
	scopes, err := resolvePruneScopes(jobs, parent, "off")
	if err != nil {
		t.Fatalf("resolvePruneScopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != parent {
		t.Fatalf("scopes = %v, want [%s]", scopes, parent)
	}
}
