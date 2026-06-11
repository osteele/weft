package daemoncontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/util"
)

const (
	Label = "com.osteele.weft.daemon"
)

type Paths struct {
	PIDFile   string
	StdoutLog string
	StderrLog string
	PlistFile string
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

func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "weft")
	return Paths{
		PIDFile:   filepath.Join(cacheDir, "daemon.pid"),
		StdoutLog: filepath.Join(cacheDir, "daemon.stdout.log"),
		StderrLog: filepath.Join(cacheDir, "daemon.stderr.log"),
		PlistFile: filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
	}
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
	live := ok && util.IsProcessAlive(pid)
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
	isWeftExecutable := filepath.Base(exe) == "weft"
	info, err := os.Stat(exe)
	if err != nil {
		return false
	}
	if metadata != nil && metadata.Executable != "" && metadata.ExecutableModTime > 0 {
		if filepath.Clean(exe) != filepath.Clean(metadata.Executable) {
			return false
		}
		return info.ModTime().Unix() > metadata.ExecutableModTime
	}
	if !isWeftExecutable {
		return false
	}
	pidInfo, err := os.Stat(paths.PIDFile)
	if err != nil {
		return false
	}
	return info.ModTime().After(pidInfo.ModTime())
}

func WritePIDFile(path string, pid int) error {
	if existing, ok, err := ReadPID(path); err != nil {
		return err
	} else if ok && util.IsProcessAlive(existing) {
		return fmt.Errorf("daemon already running with PID %d", existing)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, pid)
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
		return 0, false, nil
	}
	if !util.IsProcessAlive(pid) {
		_ = os.Remove(paths.PIDFile)
		return pid, false, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return pid, true, err
	}
	if err := proc.Signal(syscall.SIGTERM); errors.Is(err, os.ErrProcessDone) {
		_ = os.Remove(paths.PIDFile)
		return pid, true, nil
	} else if err != nil {
		return pid, true, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !util.IsProcessAlive(pid) {
			_ = os.Remove(paths.PIDFile)
			return pid, true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := proc.Signal(syscall.SIGKILL); errors.Is(err, os.ErrProcessDone) {
		_ = os.Remove(paths.PIDFile)
		return pid, true, nil
	} else if err != nil {
		return pid, true, err
	}
	deadline = time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !util.IsProcessAlive(pid) {
			_ = os.Remove(paths.PIDFile)
			return pid, true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return pid, true, fmt.Errorf("daemon PID %d did not exit within %s after SIGKILL", pid, timeout)
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
