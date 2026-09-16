package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

func writeFakeRclone(t *testing.T, dir string) string {
	t.Helper()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake rclone bin dir: %v", err)
	}

	script := `#!/bin/sh
if [ "$1" != "cat" ]; then
  echo "unexpected command: $*" >&2
  exit 1
fi

case "$2" in
  r2:test-bucket/missing)
    echo "ERROR : object not found" >&2
    exit 1
    ;;
  r2:test-bucket/broken)
    echo "permission denied" >&2
    exit 1
    ;;
  r2:test-bucket/value)
    printf 'hello world\n'
    ;;
  *)
    echo "unexpected path: $2" >&2
    exit 1
    ;;
esac
`
	path := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	return binDir
}

func TestR2Get_MissingKeyReturnsEmpty(t *testing.T) {
	fakeBinDir := writeFakeRclone(t, t.TempDir())
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := r2Get("test-bucket", "missing")
	if err != nil {
		t.Fatalf("r2Get missing: %v", err)
	}
	if got != "" {
		t.Fatalf("r2Get missing = %q, want empty", got)
	}
}

func TestR2Get_PropagatesNonMissingErrors(t *testing.T) {
	fakeBinDir := writeFakeRclone(t, t.TempDir())
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := r2Get("test-bucket", "broken")
	if err == nil {
		t.Fatal("expected error for non-missing r2 failure")
	}
	if got != "" {
		t.Fatalf("r2Get broken = %q, want empty", got)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want permission denied", err)
	}
}

func TestR2RecoveryRequestsRespectCancellation(t *testing.T) {
	fakeBinDir := writeFakeRclone(t, t.TempDir())
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if value, err := r2GetContext(context.Background(), "test-bucket", "value"); err != nil || value != "hello world" {
		t.Fatalf("positive control: value=%q, error=%v", value, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r2GetContext(ctx, "test-bucket", "value"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired recovery still read R2: %v", err)
	}
	if err := r2PutReaderContext(ctx, "test-bucket", "value", strings.NewReader("0")); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired recovery still published to R2: %v", err)
	}
}

type recordingPublicationManager struct {
	captures []runner.PostJobCapture
}

func (m *recordingPublicationManager) WaitForWorkdir(string) {}
func (m *recordingPublicationManager) WaitForAll()           {}
func (m *recordingPublicationManager) StartPostJob(capture runner.PostJobCapture) {
	m.captures = append(m.captures, capture)
}

func TestRecoverInventoryPublicationRetriesMissingDrainAfterWaiterExit(t *testing.T) {
	previousGet, previousPut := inventoryPublicationGet, inventoryPublicationPut
	t.Cleanup(func() {
		inventoryPublicationGet, inventoryPublicationPut = previousGet, previousPut
	})
	var gotGetKey, gotPutKey, gotPutContent string
	inventoryPublicationGet = func(_ context.Context, _, key string) (string, error) {
		gotGetKey = key
		return "", nil
	}
	inventoryPublicationPut = func(_ context.Context, _, key, content string) error {
		gotPutKey, gotPutContent = key, content
		return nil
	}
	manager := &recordingPublicationManager{}
	capture := runner.PostJobCapture{
		JobID: 81, RunID: 17, WorkDir: t.TempDir(), ExitCode: 0,
		StartTime: 100, EndTime: 200,
		OutputFiles: []runner.OutputFile{{
			RelPath: ".agent-execution/results/model-call-9.json", SizeBytes: 42,
		}},
	}

	recoverInventoryPublication(context.Background(), "bucket", manager, capture)

	if gotGetKey != r2keys.JobAttemptPublicationReport(81, 17) {
		t.Fatalf("recovery read %q, want attempt publication report", gotGetKey)
	}
	if len(manager.captures) != 1 || manager.captures[0].RunID != 17 ||
		len(manager.captures[0].OutputFiles) != 1 {
		t.Fatalf("publication captures = %+v, want exact completed attempt", manager.captures)
	}
	if gotPutKey != r2keys.JobAttemptComplete(81, 17) || gotPutContent != "0" {
		t.Fatalf("completion repair = (%q, %q), want attempt marker with exit 0", gotPutKey, gotPutContent)
	}
}

func TestRecoverInventoryPublicationReadyDrainIsTerminal(t *testing.T) {
	previousGet, previousPut := inventoryPublicationGet, inventoryPublicationPut
	t.Cleanup(func() {
		inventoryPublicationGet, inventoryPublicationPut = previousGet, previousPut
	})
	inventoryPublicationGet = func(_ context.Context, _, _ string) (string, error) {
		return `{"sequence":3,"facets":{"drain_state":"ready"}}`, nil
	}
	putCalls := 0
	inventoryPublicationPut = func(context.Context, string, string, string) error {
		putCalls++
		return nil
	}
	manager := &recordingPublicationManager{}

	recoverInventoryPublication(context.Background(), "bucket", manager, runner.PostJobCapture{
		JobID: 82, RunID: 18, WorkDir: t.TempDir(), ExitCode: 0,
	})

	if len(manager.captures) != 0 || putCalls != 0 {
		t.Fatalf("ready drain retried publication: captures=%+v puts=%d", manager.captures, putCalls)
	}
}
