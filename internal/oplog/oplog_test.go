package oplog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileLogger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	logger, err := newFileLogger(path, DefaultMaxSize)
	if err != nil {
		t.Fatalf("newFileLogger failed: %v", err)
	}
	defer logger.Close()

	// Log a simple operation
	logger.Log(OpCLICommand, WithDetail("run --host host-beta"))

	// Log a job operation
	logger.LogJob(OpJobStart, 1234, "host-beta", WithDetail("starting via TUI"))

	// Log with error
	logger.LogJob(OpJobStartFailed, 1234, "host-beta", WithError(errors.New("connection timeout")))

	// Log with duration
	logger.LogJob(OpJobComplete, 1234, "host-beta", WithDuration(5*time.Second))

	logger.Close()

	// Read and verify
	entries, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries failed: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Verify first entry
	if entries[0].Operation != OpCLICommand {
		t.Errorf("entry[0].Operation = %q, want %q", entries[0].Operation, OpCLICommand)
	}
	if entries[0].Detail != "run --host host-beta" {
		t.Errorf("entry[0].Detail = %q, want %q", entries[0].Detail, "run --host host-beta")
	}

	// Verify job entry
	if entries[1].JobID != 1234 {
		t.Errorf("entry[1].JobID = %d, want 1234", entries[1].JobID)
	}
	if entries[1].Host != "host-beta" {
		t.Errorf("entry[1].Host = %q, want host-beta", entries[1].Host)
	}

	// Verify error entry
	if entries[2].Error != "connection timeout" {
		t.Errorf("entry[2].Error = %q, want 'connection timeout'", entries[2].Error)
	}

	// Verify duration entry
	if entries[3].Duration != 5000 {
		t.Errorf("entry[3].Duration = %d, want 5000", entries[3].Duration)
	}
}

func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Create a file larger than max size
	maxSize := int64(100)
	content := strings.Repeat("x", 150)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Initialize logger (should trigger rotation)
	logger, err := newFileLogger(path, maxSize)
	if err != nil {
		t.Fatalf("newFileLogger failed: %v", err)
	}
	defer logger.Close()

	// Backup should exist
	backup := path + ".1"
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		t.Error("backup file should exist after rotation")
	}

	// Original should be empty/new
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() >= maxSize {
		t.Errorf("new file size = %d, should be less than %d", info.Size(), maxSize)
	}
}

func TestFilterEntries(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		{Time: now.Add(-2 * time.Hour), Operation: OpJobStart, JobID: 100, Host: "host1"},
		{Time: now.Add(-1 * time.Hour), Operation: OpJobComplete, JobID: 100, Host: "host1"},
		{Time: now.Add(-30 * time.Minute), Operation: OpJobStart, JobID: 200, Host: "host2"},
		{Time: now.Add(-10 * time.Minute), Operation: OpJobFail, JobID: 200, Host: "host2", Error: "timeout"},
	}

	tests := []struct {
		name    string
		opts    FilterOptions
		wantLen int
	}{
		{"no filter", FilterOptions{}, 4},
		{"by job ID", FilterOptions{JobID: 100}, 2},
		{"by host", FilterOptions{Host: "host2"}, 2},
		{"by operation", FilterOptions{Operation: OpJobStart}, 2},
		{"since 1h ago", FilterOptions{Since: now.Add(-1 * time.Hour)}, 3},
		{"errors only", FilterOptions{ErrorsOnly: true}, 1},
		{"combined", FilterOptions{JobID: 200, ErrorsOnly: true}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FilterEntries(entries, tt.opts)
			if len(result) != tt.wantLen {
				t.Errorf("FilterEntries returned %d entries, want %d", len(result), tt.wantLen)
			}
		})
	}
}

func TestEntryJSONFormat(t *testing.T) {
	entry := Entry{
		Time:      time.Date(2025, 12, 31, 15, 36, 12, 0, time.UTC),
		Operation: OpJobStart,
		JobID:     1384,
		Host:      "host-beta",
		Detail:    "starting queued job",
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Verify short field names are used
	s := string(data)
	if !strings.Contains(s, `"t":`) {
		t.Error("JSON should use short field name 't' for time")
	}
	if !strings.Contains(s, `"op":`) {
		t.Error("JSON should use short field name 'op' for operation")
	}
	if !strings.Contains(s, `"job":`) {
		t.Error("JSON should use short field name 'job' for jobID")
	}

	// Verify omitempty works
	entryNoJob := Entry{
		Time:      time.Now(),
		Operation: OpCLICommand,
	}
	data2, _ := json.Marshal(entryNoJob)
	if strings.Contains(string(data2), `"job":`) {
		t.Error("JSON should omit job field when zero")
	}
}

func TestGlobalLogger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.log")

	// Initialize global logger
	if err := Init(path, DefaultMaxSize); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Use global functions
	Log(OpCLICommand, WithDetail("test command"))
	LogJob(OpJobStart, 999, "testhost", WithDetail("test job"))

	// Close and read
	Close()

	entries, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries failed: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestGlobalLoggerSyncKeepsLoggerActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.log")

	if err := Init(path, DefaultMaxSize); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	Log(OpCLICommand, WithDetail("before sync"))
	if err := Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	Log(OpCLICommand, WithDetail("after sync"))

	if err := Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	entries, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries after sync, got %d", len(entries))
	}
	if entries[0].Detail != "before sync" || entries[1].Detail != "after sync" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestNoopLoggerBeforeInit(t *testing.T) {
	// Reset to noop logger
	defaultMu.Lock()
	defaultLogger = &noopLogger{}
	defaultMu.Unlock()

	// These should not panic
	Log(OpCLICommand, WithDetail("should be ignored"))
	LogJob(OpJobStart, 123, "host", WithDetail("also ignored"))

	// Close should also be safe
	Close()
}
