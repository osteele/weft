package ops

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

func TestRecordQueuedJobStoresOnlyVerifiedCanonicalProjectRoot(t *testing.T) {
	database := db.SetupTestDB(t)
	base := t.TempDir()
	realRoot := filepath.Join(base, "project")
	nested := filepath.Join(realRoot, "experiments")
	if err := os.MkdirAll(filepath.Join(realRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "project-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	workingDir := filepath.Join(linkRoot, "experiments")
	canonicalRoot, err := workdir.CanonicalPath(realRoot)
	if err != nil {
		t.Fatal(err)
	}

	record := func(project string) int64 {
		t.Helper()
		id, err := RecordQueuedJob(database, QueueJobParams{
			Host:       "cool30",
			WorkingDir: workingDir,
			Command:    "echo ok",
			Project:    project,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	matching := record("project")
	overridden := record("logical-owner")
	nonRepoDir := filepath.Join(base, "plain-directory")
	if err := os.MkdirAll(nonRepoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nonRepoID, err := RecordQueuedJob(database, QueueJobParams{
		Host:       "cool30",
		WorkingDir: nonRepoDir,
		Command:    "echo ok",
		Project:    "plain-directory",
	})
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
	if got := readRoot(matching); !got.Valid || got.String != canonicalRoot {
		t.Fatalf("matching project_root = %+v, want %q", got, canonicalRoot)
	}
	if got := readRoot(overridden); got.Valid {
		t.Fatalf("overridden project_root = %q, want NULL", got.String)
	}
	if got := readRoot(nonRepoID); got.Valid {
		t.Fatalf("non-repository project_root = %q, want NULL", got.String)
	}
}
