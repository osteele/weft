package daemoncontrol

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const stalePID = 99999999

func TestWritePIDFileRejectsLivePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePIDFile(path, os.Getpid()); err == nil {
		t.Fatal("WritePIDFile accepted an existing live PID")
	}
}

func TestWritePIDFileOverwritesStalePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", stalePID)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePIDFile(path, stalePID); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	pid, ok, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if !ok || pid != stalePID {
		t.Fatalf("ReadPID = %d, %v; want %d, true", pid, ok, stalePID)
	}
}

func TestRemovePIDFileIfOwn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.pid")
	if err := WritePIDFile(path, stalePID); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	if err := RemovePIDFileIfOwn(path, 999); err != nil {
		t.Fatalf("RemovePIDFileIfOwn other: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pidfile removed by non-owner: %v", err)
	}
	if err := RemovePIDFileIfOwn(path, stalePID); err != nil {
		t.Fatalf("RemovePIDFileIfOwn owner: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pidfile still exists after owner removal: %v", err)
	}
}

func TestCurrentStatusClassifiesStalePID(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		PIDFile:   filepath.Join(dir, "daemon.pid"),
		StdoutLog: filepath.Join(dir, "out.log"),
		StderrLog: filepath.Join(dir, "err.log"),
		PlistFile: filepath.Join(dir, "daemon.plist"),
	}
	if err := WritePIDFile(paths.PIDFile, stalePID); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	status, err := CurrentStatus(paths)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if !status.HasPID || status.Live || !status.Stale {
		t.Fatalf("status = %+v, want stale PID", status)
	}
}
