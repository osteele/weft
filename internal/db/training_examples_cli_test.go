package db

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestTrainingExamplesViewParsesInSystemSQLiteCLI(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}

	tmpFile, err := os.CreateTemp("", "weft-db-cli-*.db")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	cleanup := SetDBPath(tmpFile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	database.Close()

	schemaCmd := exec.Command("sqlite3", tmpFile.Name(), ".schema training_examples")
	schemaOut, err := schemaCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 .schema training_examples failed: %v\n%s", err, schemaOut)
	}
	if !strings.Contains(string(schemaOut), "CREATE VIEW training_examples AS") {
		t.Fatalf("sqlite3 .schema training_examples output missing view definition:\n%s", schemaOut)
	}

	queryCmd := exec.Command("sqlite3", tmpFile.Name(), "SELECT COUNT(*) FROM training_examples;")
	queryOut, err := queryCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 SELECT COUNT(*) FROM training_examples failed: %v\n%s", err, queryOut)
	}
	if got := strings.TrimSpace(string(queryOut)); got != "0" {
		t.Fatalf("sqlite3 SELECT COUNT(*) FROM training_examples = %q, want 0", got)
	}
}
