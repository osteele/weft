package cmd

import (
	"database/sql"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
)

func setupSyncTestDB(t *testing.T) *sql.DB {
	t.Helper()

	tmpfile, err := os.CreateTemp("", "remote-jobs-sync-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpfile.Close()

	cleanup := db.SetDBPath(tmpfile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpfile.Name())
	})

	database, err := db.Open()
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		database.Close()
	})
	return database
}

func TestSyncHostWithTimeoutExecutesDeferredBeforeSync(t *testing.T) {
	database := setupSyncTestDB(t)

	jobID, err := db.RecordJobStarting(database, "host-sync", "/tmp", "echo sync", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	if err := db.AddDeferredOperation(database, "host-sync", db.OpRunJob, jobID, "", `{}`); err != nil {
		t.Fatalf("add deferred: %v", err)
	}

	var order []string

	origExec := executeDeferredOpsFunc
	executeDeferredOpsFunc = func(dbConn *sql.DB, host string, opts ops.ExecuteOptions) (ops.ExecutionResult, error) {
		order = append(order, "deferred")
		if host != "host-sync" {
			t.Fatalf("unexpected host %s", host)
		}
		return ops.ExecutionResult{}, nil
	}
	defer func() { executeDeferredOpsFunc = origExec }()

	origSync := syncJobQuickFunc
	syncJobQuickFunc = func(dbConn *sql.DB, job *db.Job, opts ops.SyncOptions) (bool, error) {
		order = append(order, "sync")
		if len(order) == 0 || order[0] != "deferred" {
			t.Fatalf("sync ran before deferred operations: %v", order)
		}
		return false, nil
	}
	defer func() { syncJobQuickFunc = origSync }()

	if _, err := syncHostWithTimeout(database, "host-sync", time.Second); err != nil {
		t.Fatalf("syncHostWithTimeout: %v", err)
	}

	if len(order) < 2 || order[0] != "deferred" || order[1] != "sync" {
		t.Fatalf("unexpected call order: %v", order)
	}
}

func TestBase64EncodingPreservesSpecialCharacters(t *testing.T) {
	// Test that base64 encoding properly handles commands with shell-special characters
	// like parentheses which caused issues with nested shell quoting
	testCases := []struct {
		name    string
		command string
	}{
		{
			name:    "parentheses in codec names",
			command: `uv run compression-lab report --codecs "v2f-buckets(64),v2f-buckets(128)"`,
		},
		{
			name:    "single quotes",
			command: `echo 'hello world'`,
		},
		{
			name:    "double quotes",
			command: `echo "hello world"`,
		},
		{
			name:    "dollar signs",
			command: `echo $HOME && ls $TMPDIR`,
		},
		{
			name:    "backticks",
			command: "echo `date`",
		},
		{
			name:    "mixed special chars",
			command: `cd ~/dir && uv run script.py --opt="value(1)" --flag='test'`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			jobLine := "123\t/home/user\t" + tc.command + "\ttest description\t\t"

			// Encode and decode
			encoded := base64.StdEncoding.EncodeToString([]byte(jobLine))
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("failed to decode: %v", err)
			}

			if string(decoded) != jobLine {
				t.Errorf("roundtrip failed:\n  got:  %q\n  want: %q", string(decoded), jobLine)
			}

			// Verify encoded string has no shell-special characters
			shellSpecial := []string{"(", ")", "'", "\"", "$", "`", "\\", ";", "&", "|", "<", ">"}
			for _, char := range shellSpecial {
				if strings.Contains(encoded, char) {
					t.Errorf("encoded string contains shell-special character %q: %s", char, encoded)
				}
			}
		})
	}
}
