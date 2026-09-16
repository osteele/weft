package cmd

import (
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

func TestReadOnlyJobListDoesNotWaitForLifecycleHooks(t *testing.T) {
	if os.Getenv("WEFT_WB136_HELPER") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadOnlyJobListDoesNotWaitForLifecycleHooks$")
		command.Env = append(os.Environ(), "WEFT_WB136_HELPER=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("read-only list lifecycle isolation: %v\n%s", err, output)
		}
		return
	}

	restoreListFlags(t)
	stubEmptyQueueStatus(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("WEFT_PPROF", "0")
	t.Setenv("WEFT_EVENT_ID", "")
	database := db.SetupTestDB(t)
	const hookID = "blocked-capture"
	if _, err := db.ClaimLifecycleHookDeliveries(database, hookID, 0, 0, time.Now(), time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	jobID, err := db.RecordQueued(database, "test-host", dir, "true", "hook isolation")
	if err != nil {
		t.Fatal(err)
	}

	hookStartedPath := filepath.Join(dir, "hook-started")
	hookReleasePath := filepath.Join(dir, "hook-release")
	eventPath := filepath.Join(dir, "event.json")
	hookCommand, err := json.Marshal([]string{
		"sh", "-c",
		`touch "$1"; while [ ! -e "$2" ]; do sleep 0.05; done; cat > "$3"; printf '%s\n' '{"schema_version":"weft-hook-ack/v1","disposition":"handled"}'`,
		"hook", hookStartedPath, hookReleasePath, eventPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	configBody := fmt.Sprintf("[[events.hooks]]\nid = %q\ncommand = %s\n", hookID, hookCommand)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetConfigPathsForTesting(configPath, filepath.Join(dir, "config.yaml")))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	os.Args = []string{
		os.Args[0], "job", "list",
		"--format", "json",
		"--limit", "500",
		"--no-sync",
		"--unprocessed",
		"--all",
		"--all-hosts",
	}
	var executeErr error
	startedAt := time.Now()
	output := captureStdout(t, func() {
		executeErr = Execute()
	})
	elapsed := time.Since(startedAt)
	if executeErr != nil {
		t.Fatal(executeErr)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("read-only list took %v with a blocked lifecycle hook configured, want < 2s", elapsed)
	}
	var document struct {
		Jobs []struct {
			JobID string `json:"job_id"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatalf("list returned invalid JSON: %v\n%s", err, output)
	}
	if len(document.Jobs) != 1 || document.Jobs[0].JobID != ids.FormatJobID(jobID) {
		t.Fatalf("list returned wrong jobs: %+v", document.Jobs)
	}
	if _, err := os.Stat(hookStartedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only list invoked lifecycle hook: stat error = %v", err)
	}
	var queuedEvents int
	if err := database.QueryRow(`
		SELECT COUNT(*)
		FROM job_lifecycle_events AS e
		JOIN lifecycle_hook_registrations AS r
		  ON e.occurred_at >= r.registered_at
		WHERE r.hook_id = ? AND e.job_id = ?`,
		hookID, jobID,
	).Scan(&queuedEvents); err != nil {
		t.Fatalf("load queued lifecycle events: %v", err)
	}
	if queuedEvents == 0 {
		t.Fatal("read-only list lost the lifecycle event queued for daemon delivery")
	}

	t.Cleanup(func() { _ = os.WriteFile(hookReleasePath, nil, 0o600) })
	daemonDone := make(chan struct{})
	go func() {
		dispatchDaemonLifecycleHooks(database, cfg)
		close(daemonDone)
	}()
	hookStartDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(hookStartedPath); err == nil {
			break
		}
		if time.Now().After(hookStartDeadline) {
			t.Fatal("daemon did not start queued lifecycle hook")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var deliveryState string
	if err := database.QueryRow(
		`SELECT state FROM lifecycle_hook_deliveries WHERE hook_id = ?`,
		hookID,
	).Scan(&deliveryState); err != nil {
		t.Fatalf("load blocked hook delivery: %v", err)
	}
	if deliveryState != "pending" {
		t.Fatalf("blocked hook delivery state = %q, want pending", deliveryState)
	}
	if err := os.WriteFile(hookReleasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-daemonDone:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not finish released lifecycle hook")
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("daemon did not deliver retained event: %v", err)
	}
	var event db.JobLifecycleEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("hook received invalid event JSON: %v\n%s", err, data)
	}
	if event.JobID != ids.FormatJobID(jobID) || event.Status != db.StatusQueued {
		t.Fatalf("hook received wrong event: %+v", event)
	}
	if err := database.QueryRow(
		`SELECT state FROM lifecycle_hook_deliveries WHERE hook_id = ?`,
		hookID,
	).Scan(&deliveryState); err != nil {
		t.Fatalf("load delivered hook state: %v", err)
	}
	if deliveryState != "delivered" {
		t.Fatalf("hook delivery state after daemon = %q, want delivered", deliveryState)
	}
}
