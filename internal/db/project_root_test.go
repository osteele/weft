package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/workdir"
)

func TestBackfillProjectRootsUsesVerifiedCachedFilesystemEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := filepath.Join(home, "code", "project")
	nested := filepath.Join(repoRoot, "experiments", "one")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	database := SetupTestDB(t)
	record := func(workingDir, project string) int64 {
		t.Helper()
		id, err := RecordQueued(database, "cool30", workingDir, "echo ok", project)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE jobs SET project = ?, project_root = NULL WHERE id = ?`, project, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	matchingA := record("~/code/project/experiments/one", "project")
	matchingB := record("~/code/project/experiments/one", "project")
	mismatch := record("~/code/project/experiments/one", "logical-owner")
	missing := record("~/code/missing", "missing")

	oldResolver := projectRepoRootResolver
	resolverCalls := 0
	projectRepoRootResolver = func(dir string) string {
		resolverCalls++
		return workdir.DetectRepoRoot(dir)
	}
	t.Cleanup(func() { projectRepoRootResolver = oldResolver })

	if err := backfillProjectRoots(database); err != nil {
		t.Fatalf("backfillProjectRoots: %v", err)
	}
	canonicalRoot, err := workdir.CanonicalPath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	readRoot := func(id int64) sql.NullString {
		t.Helper()
		var root sql.NullString
		if err := database.QueryRow(`SELECT project_root FROM jobs WHERE id = ?`, id).Scan(&root); err != nil {
			t.Fatal(err)
		}
		return root
	}
	for _, id := range []int64{matchingA, matchingB} {
		if got := readRoot(id); !got.Valid || got.String != canonicalRoot {
			t.Errorf("job %d project_root = %+v, want %q", id, got, canonicalRoot)
		}
	}
	for _, id := range []int64{mismatch, missing} {
		if got := readRoot(id); got.Valid {
			t.Errorf("job %d project_root = %q, want NULL", id, got.String)
		}
	}
	if resolverCalls != 1 {
		t.Fatalf("repo-root resolver calls = %d, want 1 for one existing directory", resolverCalls)
	}
	t.Log("fixture backfill coverage: 2/4 rows (50%); mismatch and missing directory remained NULL")

	if err := backfillProjectRoots(database); err != nil {
		t.Fatalf("idempotent backfillProjectRoots: %v", err)
	}
	for _, id := range []int64{matchingA, matchingB} {
		if got := readRoot(id); !got.Valid || got.String != canonicalRoot {
			t.Errorf("job %d changed after second backfill: %+v", id, got)
		}
	}
}
