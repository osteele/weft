package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
)

func writeFakeDirenv(t *testing.T, dir, value string) string {
	t.Helper()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake direnv bin dir: %v", err)
	}

	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  allow)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"  exec)\n" +
		"    shift 2\n" +
		"    if [ \"$1\" = \"env\" ] && [ \"$2\" = \"-0\" ]; then\n" +
		"      printf 'FOO=" + value + "\\0'\n" +
		"      exit 0\n" +
		"    fi\n" +
		"    echo \"unexpected exec args: $*\" >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"esac\n" +
		"echo \"unexpected args: $*\" >&2\n" +
		"exit 1\n"
	path := filepath.Join(binDir, "direnv")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake direnv: %v", err)
	}
	return binDir
}

func TestRunSingleJob_EchoHello(t *testing.T) {
	logDir := t.TempDir()

	cfg := SingleJobConfig{
		JobID:          42,
		Job:            opsqueue.CommandJob{Cmd: "echo hello"},
		LogDir:         logDir,
		SampleInterval: 100 * time.Millisecond,
		SkipProbes:     true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", ei.ExitCode)
	}

	// Check that key files were written
	paths := NewJobPaths(logDir, 42)

	for _, check := range []struct {
		name string
		path string
	}{
		{"log", paths.Log},
		{"status", paths.Status},
		{"meta", paths.Meta},
		{"completion", paths.Completion},
		{"phases", paths.Phases},
	} {
		if _, err := os.Stat(check.path); err != nil {
			t.Errorf("%s file not found: %s", check.name, check.path)
		}
	}

	// Verify status file contains exit code 0
	code, ok := ReadStatusFile(paths.Status)
	if !ok {
		t.Fatal("could not read status file")
	}
	if code != 0 {
		t.Errorf("status file exit code = %d, want 0", code)
	}

	// Verify log contains "hello"
	logData, err := os.ReadFile(paths.Log)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if got := string(logData); !strings.Contains(got, "hello") {
		t.Errorf("log does not contain 'hello': %s", got)
	}
}

func TestRunSingleJob_FailingCommand(t *testing.T) {
	logDir := t.TempDir()

	cfg := SingleJobConfig{
		JobID:          99,
		Job:            opsqueue.CommandJob{Cmd: "exit 42"},
		LogDir:         logDir,
		SampleInterval: 100 * time.Millisecond,
		SkipProbes:     true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", ei.ExitCode)
	}

	// Check failure reason file exists
	paths := NewJobPaths(logDir, 99)
	reason := ReadFailureReasonFile(paths.FailureReason)
	if reason == "" {
		t.Error("expected failure reason to be written")
	}
}

func TestRunSingleJob_WorkingDir(t *testing.T) {
	logDir := t.TempDir()
	workDir := t.TempDir()

	// Create a file in the work dir to verify we're in the right place
	os.WriteFile(filepath.Join(workDir, "marker.txt"), []byte("found"), 0644)

	cfg := SingleJobConfig{
		JobID:          7,
		Job:            opsqueue.CommandJob{Cmd: "cat marker.txt"},
		LogDir:         logDir,
		WorkingDir:     workDir,
		SampleInterval: 100 * time.Millisecond,
		SkipProbes:     true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", ei.ExitCode)
	}

	logData, _ := os.ReadFile(NewJobPaths(logDir, 7).Log)
	if !strings.Contains(string(logData), "found") {
		t.Error("expected 'found' in log output")
	}
}

func TestRunSingleJob_OnPhase(t *testing.T) {
	logDir := t.TempDir()

	var phases []string
	cfg := SingleJobConfig{
		JobID:          50,
		Job:            opsqueue.CommandJob{Cmd: "echo phase-test"},
		LogDir:         logDir,
		SampleInterval: 100 * time.Millisecond,
		SkipProbes:     true,
		OnPhase: func(phase string) {
			phases = append(phases, phase)
		},
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", ei.ExitCode)
	}

	if len(phases) < 2 {
		t.Fatalf("expected at least 2 phase callbacks, got %d: %v", len(phases), phases)
	}
	if phases[0] != "setup" {
		t.Errorf("first phase = %q, want %q", phases[0], "setup")
	}
	if phases[1] != "running" {
		t.Errorf("second phase = %q, want %q", phases[1], "running")
	}
}

func TestRunSingleJob_DirenvEnvAppliedAndJobEnvOverrides(t *testing.T) {
	logDir := t.TempDir()
	workDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(workDir, ".envrc"), []byte("export FOO=from_direnv\n"), 0o644); err != nil {
		t.Fatalf("write .envrc: %v", err)
	}
	fakeBinDir := writeFakeDirenv(t, t.TempDir(), "from_direnv")
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := SingleJobConfig{
		JobID:          51,
		Job:            opsqueue.CommandJob{Cmd: `printf '%s\n' "$FOO"`, Env: []string{"FOO=from_job"}},
		LogDir:         logDir,
		WorkingDir:     workDir,
		SampleInterval: 100 * time.Millisecond,
		SkipProbes:     true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", ei.ExitCode)
	}

	logData, err := os.ReadFile(NewJobPaths(logDir, 51).Log)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "from_job") {
		t.Fatalf("expected log to contain overridden direnv value, got %s", logData)
	}
}

func TestRunSingleJob_EmptyCommand(t *testing.T) {
	logDir := t.TempDir()

	cfg := SingleJobConfig{
		JobID:      1,
		Job:        opsqueue.CommandJob{},
		LogDir:     logDir,
		SkipProbes: true,
	}

	ei, err := RunSingleJob(cfg)
	if err == nil {
		t.Fatal("expected error for empty command")
	}
	if ei.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", ei.ExitCode)
	}
}
