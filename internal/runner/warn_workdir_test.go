package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWarnIfWorkdirMissingEnv_MissingDir(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(logFile, []byte(""), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	WarnIfWorkdirMissingEnv("/nonexistent/path", 99, logFile)

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "is missing") {
		t.Errorf("log does not mention missing workdir; got: %q", data)
	}
}

func TestWarnIfWorkdirMissingEnv_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(logFile, []byte(""), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	WarnIfWorkdirMissingEnv(dir, 99, logFile)
	data, _ := os.ReadFile(logFile)
	if !strings.Contains(string(data), "is empty") {
		t.Errorf("log does not warn about empty workdir; got: %q", data)
	}
}

func TestWarnIfWorkdirMissingEnv_PyprojectWithoutLockOrVenv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	logFile := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(logFile, []byte(""), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	WarnIfWorkdirMissingEnv(dir, 99, logFile)
	data, _ := os.ReadFile(logFile)
	if !strings.Contains(string(data), "uv.lock") {
		t.Errorf("log does not mention uv.lock; got: %q", data)
	}
}

func TestWarnIfWorkdirMissingEnv_HealthyDirSilent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(""), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	logFile := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(logFile, []byte(""), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	WarnIfWorkdirMissingEnv(dir, 99, logFile)
	data, _ := os.ReadFile(logFile)
	if len(data) != 0 {
		t.Errorf("expected no log output for healthy workdir; got: %q", data)
	}
}

func TestWarnIfWorkdirMissingEnv_NonPythonDirSilent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	logFile := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(logFile, []byte(""), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	WarnIfWorkdirMissingEnv(dir, 99, logFile)
	data, _ := os.ReadFile(logFile)
	if len(data) != 0 {
		t.Errorf("expected no log output for non-Python workdir; got: %q", data)
	}
}
