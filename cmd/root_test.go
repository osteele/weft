package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func TestIsUsageError_RequiredFlag(t *testing.T) {
	err := errors.New(`required flag(s) "older-than" not set`)
	if !isUsageError(err) {
		t.Fatalf("expected required flag error to be treated as usage")
	}
}

func TestRewriteRootArgs_ExpandsAlias(t *testing.T) {
	cfg := &config.Config{
		Aliases: map[string]string{
			"uj": "job list --group-by status --unprocessed --watch",
		},
	}

	got := rewriteRootArgs([]string{"weft", "uj", "--help"}, cfg)
	want := []string{"weft", "job", "list", "--group-by", "status", "--unprocessed", "--watch", "--help"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteRootArgs() = %v, want %v", got, want)
	}
}

func TestRewriteRootArgs_ExpandsAliasAfterRootFlag(t *testing.T) {
	cfg := &config.Config{
		Aliases: map[string]string{
			"uj": "job list --group-by status --unprocessed --watch",
		},
	}

	got := rewriteRootArgs([]string{"weft", "--verbose", "uj"}, cfg)
	want := []string{"weft", "--verbose", "job", "list", "--group-by", "status", "--unprocessed", "--watch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteRootArgs() = %v, want %v", got, want)
	}
}

func TestRewriteRootArgs_AppliesDefaultCommandBeforeAliases(t *testing.T) {
	cfg := &config.Config{
		DefaultCommand: "uj",
		Aliases: map[string]string{
			"uj": "job list --group-by status --unprocessed --watch",
		},
	}

	got := rewriteRootArgs([]string{"weft"}, cfg)
	want := []string{"weft", "job", "list", "--group-by", "status", "--unprocessed", "--watch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteRootArgs() = %v, want %v", got, want)
	}
}

func TestExecuteDoesNotDrainLifecycleHooks(t *testing.T) {
	if os.Getenv("WEFT_ROOT_HOOK_HELPER") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecuteDoesNotDrainLifecycleHooks$")
		command.Env = append(os.Environ(), "WEFT_ROOT_HOOK_HELPER=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("CLI lifecycle isolation: %v\n%s", err, output)
		}
		return
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("WEFT_PPROF", "0")
	t.Setenv("WEFT_EVENT_ID", "")
	database := db.SetupTestDB(t)
	if _, err := db.ClaimLifecycleHookDeliveries(database, "capture", 0, 0, time.Now(), time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	jobID, err := db.RecordQueued(database, "test-host", dir, "true", "hook isolation")
	if err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(dir, "events.jsonl")
	hookCommand, err := json.Marshal([]string{
		"sh", "-c",
		`cat >> "$1"; printf '%s\n' '{"schema_version":"weft-hook-ack/v1","disposition":"handled"}'`,
		"hook", eventPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	configBody := fmt.Sprintf("[[events.hooks]]\nid = \"capture\"\ncommand = %s\n", hookCommand)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetConfigPathsForTesting(configPath, filepath.Join(dir, "config.yaml")))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	os.Args = []string{os.Args[0], "job", "inspect", ids.FormatJobID(jobID), "--json"}
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	if err := Execute(); err != nil {
		t.Fatal(err)
	}
	var record normalizedJobRecord
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("inspect returned invalid JSON: %v\n%s", err, &output)
	}
	if record.ID != ids.FormatJobID(jobID) || record.Status != db.StatusQueued {
		t.Fatalf("inspect returned the wrong job: %+v", record)
	}
	if _, err := os.Stat(eventPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only CLI delivered a lifecycle hook: stat error = %v", err)
	}

	// The same retry drain used by the daemon must still deliver the retained event.
	dispatchConfiguredLifecycleHooks(database, cfg)
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("retained event was not delivered: %v", err)
	}
	var event db.JobLifecycleEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("hook received invalid event JSON: %v\n%s", err, data)
	}
	if event.JobID != record.ID || event.Status != record.Status {
		t.Fatalf("hook received the wrong event: %+v", event)
	}
	dispatchConfiguredLifecycleHooks(database, cfg)
	after, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, after) {
		t.Fatal("acknowledged event was delivered again")
	}
}
