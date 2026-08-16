package daemoncontrol

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemonapi"
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

func TestAcquireLockRejectsSecondHolder(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{LockFile: filepath.Join(dir, "daemon.lock")}
	lock, err := AcquireLock(paths)
	if err != nil {
		t.Fatalf("AcquireLock first: %v", err)
	}
	defer lock.Close()
	if second, err := AcquireLock(paths); err == nil {
		second.Close()
		t.Fatal("AcquireLock allowed a second holder")
	}
}

func TestLaunchdEnvironmentPathIncludesProviderCLIDirs(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/homebrew/bin")

	got := launchdEnvironmentPath("/Users/tester")
	for _, want := range []string{
		"/Users/tester/.local/bin",
		"/Users/tester/bin",
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
	} {
		if !pathListContains(got, want) {
			t.Fatalf("PATH %q missing %q", got, want)
		}
	}
	if countPathListEntry(got, "/opt/homebrew/bin") != 1 {
		t.Fatalf("PATH %q should contain /opt/homebrew/bin once", got)
	}
}

func TestEnsureCurrentRepairsPIDFileFromLiveSocket(t *testing.T) {
	dir := t.TempDir()
	socketPath := shortTestSocketPath(t)
	paths := Paths{
		PIDFile:    filepath.Join(dir, "daemon.pid"),
		LockFile:   filepath.Join(dir, "daemon.lock"),
		SocketFile: socketPath,
		StdoutLog:  filepath.Join(dir, "out.log"),
		StderrLog:  filepath.Join(dir, "err.log"),
		PlistFile:  filepath.Join(dir, "daemon.plist"),
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	stat, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("Stat executable: %v", err)
	}
	server, err := daemonapi.StartServerWithOptions(t.Context(), nil, paths.SocketFile, daemonapi.ServerOptions{
		Info: daemonapi.DaemonInfo{
			PID:               os.Getpid(),
			Version:           "test-version",
			Executable:        exe,
			ExecutableModTime: stat.ModTime().Unix(),
			StartedAt:         time.Now().Unix(),
		},
	})
	if err != nil {
		t.Fatalf("StartServerWithOptions: %v", err)
	}
	defer server.Close()

	status, action, err := EnsureCurrent(paths, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	if action != EnsureNoop || !status.Live || status.PID != os.Getpid() {
		t.Fatalf("EnsureCurrent = status %+v action %q, want repaired live noop", status, action)
	}
	pid, ok, err := ReadPID(paths.PIDFile)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if !ok || pid != os.Getpid() {
		t.Fatalf("ReadPID = %d, %v; want %d, true", pid, ok, os.Getpid())
	}
	metadata, err := ReadMetadata(paths)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if metadata == nil || metadata.PID != os.Getpid() || metadata.Version != "test-version" {
		t.Fatalf("metadata = %+v, want repaired daemon metadata", metadata)
	}
}

func pathListContains(pathList, want string) bool {
	return countPathListEntry(pathList, want) > 0
}

func countPathListEntry(pathList, want string) int {
	count := 0
	for _, entry := range filepath.SplitList(pathList) {
		if entry == want {
			count++
		}
	}
	return count
}

func TestEnsureSocketAvailableRejectsLiveWeftSocket(t *testing.T) {
	paths := Paths{SocketFile: shortTestSocketPath(t)}
	server, err := daemonapi.StartServerWithOptions(t.Context(), nil, paths.SocketFile, daemonapi.ServerOptions{
		Info: daemonapi.DaemonInfo{PID: os.Getpid(), Version: "test-version"},
	})
	if err != nil {
		t.Fatalf("StartServerWithOptions: %v", err)
	}
	defer server.Close()

	if err := EnsureSocketAvailable(paths, 100*time.Millisecond); err == nil {
		t.Fatal("EnsureSocketAvailable accepted a live daemon socket")
	}
}

func TestEnsureSocketAvailableRemovesStaleSocketPath(t *testing.T) {
	paths := Paths{SocketFile: shortTestSocketPath(t)}
	listener, err := net.Listen("unix", paths.SocketFile)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}

	if err := EnsureSocketAvailable(paths, 100*time.Millisecond); err != nil {
		t.Fatalf("EnsureSocketAvailable: %v", err)
	}
	if _, err := os.Lstat(paths.SocketFile); !os.IsNotExist(err) {
		t.Fatalf("stale socket path still exists: %v", err)
	}
}

func TestEnsureSocketAvailablePreservesSocketOnUnknownDialFailure(t *testing.T) {
	paths := Paths{SocketFile: shortTestSocketPath(t)}
	listener, err := net.Listen("unix", paths.SocketFile)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	oldDial := dialSocketTimeout
	dialSocketTimeout = func(string, string, time.Duration) (net.Conn, error) {
		return nil, fmt.Errorf("temporary dial failure")
	}
	t.Cleanup(func() { dialSocketTimeout = oldDial })

	if err := EnsureSocketAvailable(paths, 100*time.Millisecond); err == nil {
		t.Fatal("EnsureSocketAvailable accepted unknown socket state")
	}
	if _, err := os.Lstat(paths.SocketFile); err != nil {
		t.Fatalf("unknown dial failure removed live socket path: %v", err)
	}
}

func TestEnsureCurrentRestartsLivePIDWithMissingSocket(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		PIDFile:    filepath.Join(dir, "daemon.pid"),
		SocketFile: filepath.Join(dir, "daemon.sock"),
		PlistFile:  filepath.Join(dir, "daemon.plist"),
	}
	if err := WritePIDFile(paths.PIDFile, os.Getpid()); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	oldRestart := restartForEnsure
	restarts := 0
	restartForEnsure = func(got Paths, _, _ time.Duration) (Status, error) {
		restarts++
		if got.SocketFile != paths.SocketFile {
			t.Fatalf("restart paths = %+v, want %+v", got, paths)
		}
		return Status{PID: 5150, Live: true}, nil
	}
	t.Cleanup(func() { restartForEnsure = oldRestart })

	status, action, err := EnsureCurrent(paths, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	if action != EnsureRestarted || restarts != 1 || status.PID != 5150 {
		t.Fatalf("EnsureCurrent = status %+v action %q restarts %d", status, action, restarts)
	}
}

func shortTestSocketPath(t *testing.T) string {
	t.Helper()
	path := fmt.Sprintf("/tmp/weft-dc-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestCurrentStatusClassifiesZombiePIDAsStale(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		PIDFile:   filepath.Join(dir, "daemon.pid"),
		StdoutLog: filepath.Join(dir, "out.log"),
		StderrLog: filepath.Join(dir, "err.log"),
		PlistFile: filepath.Join(dir, "daemon.plist"),
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessExitImmediately")
	cmd.Env = append(os.Environ(), "WEFT_DAEMONCONTROL_HELPER=exit-immediately")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for !processIsZombie(cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Skip("could not observe helper process as zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := WritePIDFile(paths.PIDFile, cmd.Process.Pid); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	status, err := CurrentStatus(paths)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if !status.HasPID || status.Live || !status.Stale {
		t.Fatalf("status = %+v, want stale zombie PID", status)
	}
	// Generous: this asserts SIGKILL escalation happens, not how fast a
	// loaded machine reaps the process. 100ms failed on CI runners.
	pid, hadProcess, err := StopPID(paths, 5*time.Second)
	if err != nil {
		t.Fatalf("StopPID: %v", err)
	}
	if pid != cmd.Process.Pid || hadProcess {
		t.Fatalf("StopPID = %d, %v; want %d, false", pid, hadProcess, cmd.Process.Pid)
	}
	if _, err := os.Stat(paths.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("pidfile still exists after StopPID: %v", err)
	}
}

func TestStopPIDKillsProcessThatIgnoresTerm(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		PIDFile:   filepath.Join(dir, "daemon.pid"),
		StdoutLog: filepath.Join(dir, "out.log"),
		StderrLog: filepath.Join(dir, "err.log"),
		PlistFile: filepath.Join(dir, "daemon.plist"),
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessIgnoreTerm")
	cmd.Env = append(os.Environ(), "WEFT_DAEMONCONTROL_HELPER=ignore-term")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	if err := WritePIDFile(paths.PIDFile, cmd.Process.Pid); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	// Generous: this asserts SIGKILL escalation happens, not how fast a
	// loaded machine reaps the process. 100ms failed on CI runners.
	pid, hadProcess, err := StopPID(paths, 5*time.Second)
	if err != nil {
		t.Fatalf("StopPID: %v", err)
	}
	if pid != cmd.Process.Pid || !hadProcess {
		t.Fatalf("StopPID = %d, %v; want %d, true", pid, hadProcess, cmd.Process.Pid)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process still running after StopPID")
	}
	if _, err := os.Stat(paths.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("pidfile still exists after StopPID: %v", err)
	}
}

func TestHelperProcessIgnoreTerm(t *testing.T) {
	if os.Getenv("WEFT_DAEMONCONTROL_HELPER") != "ignore-term" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	select {}
}

func TestHelperProcessExitImmediately(t *testing.T) {
	if os.Getenv("WEFT_DAEMONCONTROL_HELPER") != "exit-immediately" {
		return
	}
	os.Exit(0)
}

func TestCurrentStatusIgnoresPIDFileFallbackOutsideWeftExecutable(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		PIDFile:   filepath.Join(dir, "daemon.pid"),
		StdoutLog: filepath.Join(dir, "out.log"),
		StderrLog: filepath.Join(dir, "err.log"),
		PlistFile: filepath.Join(dir, "daemon.plist"),
	}
	if err := os.WriteFile(paths.PIDFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(paths.PIDFile, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	status, err := CurrentStatus(paths)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if !status.Live || status.ActiveBinaryStale {
		t.Fatalf("status = %+v, want live non-stale test binary", status)
	}
}

func TestCurrentStatusDetectsStaleActiveBinaryFromMetadata(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		PIDFile:   filepath.Join(dir, "daemon.pid"),
		StdoutLog: filepath.Join(dir, "out.log"),
		StderrLog: filepath.Join(dir, "err.log"),
		PlistFile: filepath.Join(dir, "daemon.plist"),
	}
	if err := os.WriteFile(paths.PIDFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	metadata := Metadata{
		PID:               os.Getpid(),
		Version:           "old",
		Executable:        exe,
		ExecutableModTime: time.Now().Add(-24 * time.Hour).Unix(),
		StartedAt:         time.Now().Add(-24 * time.Hour).Unix(),
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(MetadataPath(paths), data, 0o644); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}

	status, err := CurrentStatus(paths)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if !status.Live || !status.ActiveBinaryStale {
		t.Fatalf("status = %+v, want live stale active binary", status)
	}
}

func TestActiveBinaryStaleDetectsDifferentCurrentWeftPath(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{PIDFile: filepath.Join(dir, "daemon.pid")}
	old := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	current := time.Now().Truncate(time.Second)

	daemonExe := filepath.Join(dir, "repo", "weft")
	if err := os.MkdirAll(filepath.Dir(daemonExe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonExe, []byte("old daemon binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(daemonExe, old, old); err != nil {
		t.Fatalf("Chtimes daemon exe: %v", err)
	}

	currentExe := filepath.Join(dir, "go", "bin", "weft")
	metadata := &Metadata{
		PID:               os.Getpid(),
		Executable:        daemonExe,
		ExecutableModTime: old.Unix(),
		StartedAt:         old.Unix(),
	}
	if !activeBinaryStaleForExecutable(paths, metadata, currentExe, current) {
		t.Fatal("different current weft path newer than daemon binary was not considered stale")
	}
}

func TestActiveBinaryStaleIgnoresSameSecondMetadataPrecision(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{PIDFile: filepath.Join(dir, "daemon.pid")}
	recorded := time.Now().Truncate(time.Second)
	actual := recorded.Add(750 * time.Millisecond)

	exe := filepath.Join(dir, "go", "bin", "weft")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("daemon binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(exe, actual, actual); err != nil {
		t.Fatalf("Chtimes exe: %v", err)
	}

	metadata := &Metadata{
		PID:               os.Getpid(),
		Executable:        exe,
		ExecutableModTime: recorded.Unix(),
		StartedAt:         recorded.Unix(),
	}
	if activeBinaryStaleForExecutable(paths, metadata, exe, actual) {
		t.Fatal("same-second executable mtime should not make daemon stale")
	}
}

func TestActiveBinaryStaleIgnoresDifferentNonWeftExecutablePath(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{PIDFile: filepath.Join(dir, "daemon.pid")}
	old := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	current := time.Now().Truncate(time.Second)

	daemonExe := filepath.Join(dir, "repo", "weft")
	if err := os.MkdirAll(filepath.Dir(daemonExe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonExe, []byte("old daemon binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(daemonExe, old, old); err != nil {
		t.Fatalf("Chtimes daemon exe: %v", err)
	}

	testExe := filepath.Join(dir, "daemoncontrol.test")
	metadata := &Metadata{
		PID:               os.Getpid(),
		Executable:        daemonExe,
		ExecutableModTime: old.Unix(),
		StartedAt:         old.Unix(),
	}
	if activeBinaryStaleForExecutable(paths, metadata, testExe, current) {
		t.Fatal("different non-weft executable path should not be considered stale")
	}
}
