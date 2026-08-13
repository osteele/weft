package db

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrainingExamplesViewParsesInSystemSQLiteCLI(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}

	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	seedCurrentTestDBFile(t, dbFile)

	schemaCmd := exec.Command("sqlite3", dbFile, ".schema training_examples")
	schemaOut, err := schemaCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 .schema training_examples failed: %v\n%s", err, schemaOut)
	}
	if !strings.Contains(string(schemaOut), "CREATE VIEW training_examples AS") {
		t.Fatalf("sqlite3 .schema training_examples output missing view definition:\n%s", schemaOut)
	}

	queryCmd := exec.Command("sqlite3", dbFile, "SELECT COUNT(*) FROM training_examples;")
	queryOut, err := queryCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 SELECT COUNT(*) FROM training_examples failed: %v\n%s", err, queryOut)
	}
	if got := strings.TrimSpace(string(queryOut)); got != "0" {
		t.Fatalf("sqlite3 SELECT COUNT(*) FROM training_examples = %q, want 0", got)
	}
}
