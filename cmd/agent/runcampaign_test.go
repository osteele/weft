package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRcloneTimeoutForBytes(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  time.Duration
	}{
		{"unknown", 0, rcloneMinTimeout},
		{"tiny file", 1024, rcloneMinTimeout},
		{"sub-floor", 100 * (1 << 20), rcloneMinTimeout},          // 100 MB → still floor
		{"500 MB checkpoint", 500 * (1 << 20), 500 * time.Second}, // ~8.3 min, well past 5min ceiling
		{"1 GB", 1024 * (1 << 20), 1024 * time.Second},
		{"negative", -1, rcloneMinTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rcloneTimeoutForBytes(tt.bytes)
			if got != tt.want {
				t.Errorf("rcloneTimeoutForBytes(%d) = %v, want %v", tt.bytes, got, tt.want)
			}
			// Regression guard: a 500 MB upload must NOT inherit the old 5-min ceiling.
			if tt.bytes >= 500*(1<<20) && got <= 5*time.Minute {
				t.Errorf("rcloneTimeoutForBytes(%d) = %v; must exceed 5m for large files", tt.bytes, got)
			}
		})
	}
}

func TestUploadLiveFileSnapshotUsesRcatFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "telemetry.jsonl")
	want := "{\"event\":\"tick\"}\n"
	if err := os.WriteFile(sourcePath, []byte(want), 0o644); err != nil {
		t.Fatalf("write live file: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake rclone bin dir: %v", err)
	}
	capturePath := filepath.Join(dir, "capture")
	argsPath := filepath.Join(dir, "args")
	rcloneScript := `#!/bin/sh
printf '%s\n' "$*" > "$RCLONE_ARGS"
if [ "$1" != "rcat" ]; then
  echo "unexpected rclone command: $*" >&2
  exit 1
fi
if [ "$2" != "r2:test-bucket/live/key.jsonl" ]; then
  echo "unexpected rclone target: $2" >&2
  exit 1
fi
cat > "$RCLONE_CAPTURE"
`
	if err := os.WriteFile(filepath.Join(binDir, "rclone"), []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RCLONE_ARGS", argsPath)
	t.Setenv("RCLONE_CAPTURE", capturePath)

	uploadLiveFileSnapshot("test-bucket", 123, sourcePath, "live/key.jsonl", "live telemetry")

	got, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured rclone stdin: %v", err)
	}
	if string(got) != want {
		t.Fatalf("uploaded stdin = %q, want %q", got, want)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured rclone args: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "rcat r2:test-bucket/live/key.jsonl" {
		t.Fatalf("rclone args = %q, want rcat target", got)
	}
}

func TestReadLogTail_SmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 8192)
	if got != content {
		t.Errorf("readLogTail() = %q, want %q", got, content)
	}
}

func TestReadLogTail_LargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write more than maxBytes
	var data []byte
	for i := 0; i < 200; i++ {
		data = append(data, []byte("this is a log line with some content\n")...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 100)
	if len(got) > 100 {
		t.Errorf("readLogTail() returned %d bytes, want <= 100", len(got))
	}
	// Should contain the end of the file
	if got[len(got)-1] != '\n' {
		t.Error("readLogTail() should end with newline")
	}
}

func TestReadLogTail_MissingFile(t *testing.T) {
	got := readLogTail("/nonexistent/path/file.log", 8192)
	if got != "" {
		t.Errorf("readLogTail() = %q, want empty string", got)
	}
}

func TestReadLogTail_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 8192)
	if got != "" {
		t.Errorf("readLogTail() = %q, want empty string", got)
	}
}
