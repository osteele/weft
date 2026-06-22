package daemoncontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/daemonapi"
	"github.com/osteele/weft/internal/util"
)

const (
	Label = "com.osteele.weft.daemon"
)

type Paths struct {
	PIDFile    string
	LockFile   string
	SocketFile string
	StdoutLog  string
	StderrLog  string
	PlistFile  string
}

type Metadata struct {
	PID               int    `json:"pid"`
	Version           string `json:"version,omitempty"`
	Executable        string `json:"executable,omitempty"`
	ExecutableModTime int64  `json:"executable_mod_time,omitempty"`
	StartedAt         int64  `json:"started_at"`
}

type Status struct {
	PID               int
	HasPID            bool
	Live              bool
	Installed         bool
	Stale             bool
	ActiveBinaryStale bool
	Metadata          *Metadata
}

type EnsureAction string

const (
	EnsureNoop      EnsureAction = ""
	EnsureStarted   EnsureAction = "started"
	EnsureRestarted EnsureAction = "restarted"
)

func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "weft")
	return Paths{
		PIDFile:    filepath.Join(cacheDir, "daemon.pid"),
		LockFile:   filepath.Join(cacheDir, "daemon.lock"),
		SocketFile: filepath.Join(cacheDir, "daemon.sock"),
		StdoutLog:  filepath.Join(cacheDir, "daemon.stdout.log"),
		StderrLog:  filepath.Join(cacheDir, "daemon.stderr.log"),
		PlistFile:  filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
	}
}

type Lock struct {
	file *os.File
}

func AcquireLock(paths Paths) (*Lock, error) {
	if paths.LockFile == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(paths.LockFile), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(paths.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("daemon lock already held: %s", paths.LockFile)
		}
		return nil, fmt.Errorf("lock daemon lock file: %w", err)
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	l.file = nil
	return err
}

func ReadPID(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, true, fmt.Errorf("invalid PID file %s", path)
	}
	return pid, true, nil
}

func CurrentStatus(paths Paths) (Status, error) {
	pid, ok, err := ReadPID(paths.PIDFile)
	if err != nil {
		return Status{HasPID: ok, Installed: IsInstalled(paths)}, err
	}
	live := ok && daemonProcessLive(pid)
	metadata, _ := ReadMetadata(paths)
	return Status{
		PID:               pid,
		HasPID:            ok,
		Live:              live,
		Installed:         IsInstalled(paths),
		Stale:             ok && !live,
		ActiveBinaryStale: live && activeBinaryStale(paths, metadata),
		Metadata:          metadata,
	}, nil
}

func MetadataPath(paths Paths) string {
	return paths.PIDFile + ".json"
}

func ReadMetadata(paths Paths) (*Metadata, error) {
	data, err := os.ReadFile(MetadataPath(paths))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

func WriteMetadata(paths Paths, pid int, version string) error {
	exe, _ := os.Executable()
	metadata := Metadata{
		PID:        pid,
		Version:    version,
		Executable: exe,
		StartedAt:  time.Now().Unix(),
	}
	if info, err := os.Stat(exe); err == nil {
		metadata.ExecutableModTime = info.ModTime().Unix()
	}
	if err := os.MkdirAll(filepath.Dir(paths.PIDFile), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(MetadataPath(paths), data, 0o644)
}

func activeBinaryStale(paths Paths, metadata *Metadata) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	info, err := os.Stat(exe)
	if err != nil {
		return false
	}
	return activeBinaryStaleForExecutable(paths, metadata, exe, info.ModTime())
}

func activeBinaryStaleForExecutable(paths Paths, metadata *Metadata, exe string, exeModTime time.Time) bool {
	isWeftExecutable := filepath.Base(exe) == "weft"
	if metadata != nil && metadata.Executable != "" && metadata.ExecutableModTime > 0 {
		if daemonInfo, err := os.Stat(metadata.Executable); err == nil && daemonInfo.ModTime().Unix() > metadata.ExecutableModTime {
			return true
		}
		if filepath.Clean(exe) == filepath.Clean(metadata.Executable) {
			return exeModTime.Unix() > metadata.ExecutableModTime
		}
		return isWeftExecutable && exeModTime.Unix() > metadata.ExecutableModTime
	}
	if !isWeftExecutable {
		return false
	}
	pidInfo, err := os.Stat(paths.PIDFile)
	if err != nil {
		return false
	}
	return exeModTime.After(pidInfo.ModTime())
}

func EnsureCurrent(paths Paths, wait time.Duration) (Status, EnsureAction, error) {
	status, err := CurrentStatus(paths)
	if err != nil {
		return status, EnsureNoop, err
	}
	if info, ok, err := SocketDaemonInfo(paths, 100*time.Millisecond); err != nil {
		return status, EnsureNoop, err
	} else if ok {
		if status.Live && status.PID != info.PID {
			if _, _, err := stopLivePID(paths, status.PID, 5*time.Second); err != nil {
				return status, EnsureNoop, err
			}
		}
		if !status.Live || status.PID != info.PID {
			if err := RepairPIDFilesFromDaemonInfo(paths, info); err != nil {
				return status, EnsureNoop, err
			}
			status, err = CurrentStatus(paths)
			if err != nil {
				return status, EnsureNoop, err
			}
		}
	}
	if status.Live && !status.ActiveBinaryStale {
		return status, EnsureNoop, nil
	}
	if status.Live && status.ActiveBinaryStale {
		status, err = Restart(paths, 5*time.Second, wait)
		return status, EnsureRestarted, err
	}
	if status.Installed {
		if err := Load(paths); err != nil {
			return status, EnsureNoop, err
		}
	} else {
		if _, err := StartDetached(paths); err != nil {
			return status, EnsureNoop, err
		}
	}
	status, err = WaitForLive(paths, wait)
	return status, EnsureStarted, err
}

func WritePIDFile(path string, pid int) error {
	if existing, ok, err := ReadPID(path); err != nil {
		return err
	} else if ok && daemonProcessLive(existing) {
		return fmt.Errorf("daemon already running with PID %d", existing)
	}
	return writePIDFileValue(path, pid)
}

func writePIDFileValue(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, pid)
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func WriteMetadataFromDaemonInfo(paths Paths, info daemonapi.DaemonInfo) error {
	metadata := Metadata{
		PID:               info.PID,
		Version:           info.Version,
		Executable:        info.Executable,
		ExecutableModTime: info.ExecutableModTime,
		StartedAt:         info.StartedAt,
	}
	if metadata.StartedAt == 0 {
		metadata.StartedAt = time.Now().Unix()
	}
	if err := os.MkdirAll(filepath.Dir(paths.PIDFile), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(MetadataPath(paths), data, 0o644)
}

func RepairPIDFilesFromDaemonInfo(paths Paths, info daemonapi.DaemonInfo) error {
	if info.PID <= 0 {
		return fmt.Errorf("daemon socket reported invalid PID %d", info.PID)
	}
	if err := writePIDFileValue(paths.PIDFile, info.PID); err != nil {
		return err
	}
	return WriteMetadataFromDaemonInfo(paths, info)
}

func RemovePIDFileIfOwn(path string, pid int) error {
	existing, ok, err := ReadPID(path)
	if err != nil {
		return err
	}
	if !ok || existing != pid {
		return nil
	}
	return os.Remove(path)
}

func RemoveMetadataFileIfOwn(paths Paths, pid int) error {
	metadata, err := ReadMetadata(paths)
	if err != nil {
		return err
	}
	if metadata == nil || metadata.PID != pid {
		return nil
	}
	if err := os.Remove(MetadataPath(paths)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return err
	}
}

func StopPID(paths Paths, timeout time.Duration) (int, bool, error) {
	pid, ok, err := ReadPID(paths.PIDFile)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return StopSocketDaemon(paths, timeout)
	}
	if !daemonProcessLive(pid) {
		_ = os.Remove(paths.PIDFile)
		socketPID, hadProcess, err := StopSocketDaemon(paths, timeout)
		if err != nil || hadProcess {
			return socketPID, hadProcess, err
		}
		return pid, false, nil
	}
	return stopLivePID(paths, pid, timeout)
}

func stopLivePID(paths Paths, pid int, timeout time.Duration) (int, bool, error) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return pid, true, err
	}
	if err := proc.Signal(syscall.SIGTERM); errors.Is(err, os.ErrProcessDone) {
		_ = RemovePIDFileIfOwn(paths.PIDFile, pid)
		return pid, true, nil
	} else if err != nil {
		return pid, true, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemonProcessLive(pid) {
			_ = RemovePIDFileIfOwn(paths.PIDFile, pid)
			return pid, true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := proc.Signal(syscall.SIGKILL); errors.Is(err, os.ErrProcessDone) {
		_ = RemovePIDFileIfOwn(paths.PIDFile, pid)
		return pid, true, nil
	} else if err != nil {
		return pid, true, err
	}
	deadline = time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemonProcessLive(pid) {
			_ = RemovePIDFileIfOwn(paths.PIDFile, pid)
			return pid, true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return pid, true, fmt.Errorf("daemon PID %d did not exit within %s after SIGKILL", pid, timeout)
}

func StopSocketDaemon(paths Paths, timeout time.Duration) (int, bool, error) {
	info, ok, err := SocketDaemonInfo(paths, 100*time.Millisecond)
	if err != nil || !ok {
		return 0, false, err
	}
	if !daemonProcessLive(info.PID) {
		_ = os.Remove(paths.SocketFile)
		_ = RemovePIDFileIfOwn(paths.PIDFile, info.PID)
		return info.PID, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), minDuration(timeout, time.Second))
	_ = daemonapi.DialShutdown(ctx, paths.SocketFile)
	cancel()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemonProcessLive(info.PID) {
			_ = RemovePIDFileIfOwn(paths.PIDFile, info.PID)
			return info.PID, true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return stopLivePID(paths, info.PID, timeout)
}

func daemonProcessLive(pid int) bool {
	if !util.IsProcessAlive(pid) {
		return false
	}
	return !processIsZombie(pid)
}

func processIsZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func StartDetached(paths Paths) (int, error) {
	if status, err := CurrentStatus(paths); err != nil {
		return 0, err
	} else if status.Live {
		return status.PID, fmt.Errorf("daemon already running with PID %d", status.PID)
	}
	if err := os.MkdirAll(filepath.Dir(paths.PIDFile), 0o755); err != nil {
		return 0, err
	}
	stdout, err := os.OpenFile(paths.StdoutLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(paths.StderrLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer stderr.Close()
	binary, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(binary, "daemon", "run")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

func EnsureSocketAvailable(paths Paths, timeout time.Duration) error {
	info, ok, err := SocketDaemonInfo(paths, timeout)
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("daemon already running on socket %s with PID %d", paths.SocketFile, info.PID)
	}
	return nil
}

func SocketDaemonInfo(paths Paths, timeout time.Duration) (daemonapi.DaemonInfo, bool, error) {
	if paths.SocketFile == "" {
		return daemonapi.DaemonInfo{}, false, nil
	}
	if _, err := os.Lstat(paths.SocketFile); errors.Is(err, os.ErrNotExist) {
		return daemonapi.DaemonInfo{}, false, nil
	} else if err != nil {
		return daemonapi.DaemonInfo{}, false, fmt.Errorf("stat daemon socket: %w", err)
	}
	conn, err := net.DialTimeout("unix", paths.SocketFile, timeout)
	if err != nil {
		if removeErr := os.Remove(paths.SocketFile); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return daemonapi.DaemonInfo{}, false, fmt.Errorf("remove stale daemon socket: %w", removeErr)
		}
		return daemonapi.DaemonInfo{}, false, nil
	}
	_ = conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	info, err := daemonapi.DialDaemonInfo(ctx, paths.SocketFile)
	if err != nil {
		return daemonapi.DaemonInfo{}, false, fmt.Errorf("daemon socket is in use but did not answer as weft: %w", err)
	}
	if info.PID <= 0 {
		return daemonapi.DaemonInfo{}, false, fmt.Errorf("daemon socket reported invalid PID %d", info.PID)
	}
	return info, true, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func Restart(paths Paths, stopTimeout, wait time.Duration) (Status, error) {
	if IsInstalled(paths) {
		if err := Unload(paths); err != nil {
			return Status{}, err
		}
	}
	if _, _, err := StopPID(paths, stopTimeout); err != nil {
		return Status{}, err
	}
	if IsInstalled(paths) {
		if err := Load(paths); err != nil {
			return Status{}, err
		}
	} else if _, err := StartDetached(paths); err != nil {
		return Status{}, err
	}
	return WaitForLive(paths, wait)
}

func WaitForLive(paths Paths, wait time.Duration) (Status, error) {
	deadline := time.Now().Add(wait)
	for {
		status, err := CurrentStatus(paths)
		if err != nil {
			return status, err
		}
		if status.Live {
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("daemon did not report running within %s", wait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
