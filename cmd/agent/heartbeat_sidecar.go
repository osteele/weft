package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/r2keys"
)

const heartbeatSidecarInterval = 30 * time.Second

type heartbeatSidecarArgs struct {
	R2Bucket   string
	InstanceID int64
	DiskPath   string
	PhaseFile  string
	FatalFile  string
	ParentPID  int
}

func runHeartbeatSidecar(args []string) {
	parsed, err := parseHeartbeatSidecarArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat-sidecar: %v\n", err)
		os.Exit(2)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	emit := func(agentAlive bool) {
		sample := collectHeartbeat(readLocalPhaseFile(parsed.PhaseFile), parsed.DiskPath)
		if parsed.ParentPID > 0 {
			sample = withAgentLiveness(sample, parsed.ParentPID, agentAlive)
		}
		sample.AgentFatal = readLocalTextFile(parsed.FatalFile)
		data, err := json.Marshal(sample)
		if err != nil {
			return
		}
		_ = r2Put(parsed.R2Bucket, r2keys.InstanceHeartbeat(parsed.InstanceID), string(data))
	}

	emit(parentProcessAlive(parsed.ParentPID))
	ticker := time.NewTicker(heartbeatSidecarInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			alive := parentProcessAlive(parsed.ParentPID)
			emit(alive)
			if !alive {
				return
			}
		}
	}
}

func parseHeartbeatSidecarArgs(args []string) (heartbeatSidecarArgs, error) {
	var parsed heartbeatSidecarArgs
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--r2-bucket="):
			parsed.R2Bucket = arg[len("--r2-bucket="):]
		case strings.HasPrefix(arg, "--instance-id="):
			id, err := strconv.ParseInt(arg[len("--instance-id="):], 10, 64)
			if err != nil {
				return parsed, fmt.Errorf("invalid --instance-id: %w", err)
			}
			parsed.InstanceID = id
		case strings.HasPrefix(arg, "--disk-path="):
			parsed.DiskPath = arg[len("--disk-path="):]
		case strings.HasPrefix(arg, "--phase-file="):
			parsed.PhaseFile = arg[len("--phase-file="):]
		case strings.HasPrefix(arg, "--fatal-file="):
			parsed.FatalFile = arg[len("--fatal-file="):]
		case strings.HasPrefix(arg, "--parent-pid="):
			pid, err := strconv.Atoi(arg[len("--parent-pid="):])
			if err != nil {
				return parsed, fmt.Errorf("invalid --parent-pid: %w", err)
			}
			parsed.ParentPID = pid
		default:
			return parsed, fmt.Errorf("unknown flag: %s", arg)
		}
	}
	if parsed.R2Bucket == "" {
		return parsed, fmt.Errorf("required: --r2-bucket")
	}
	if parsed.InstanceID <= 0 {
		return parsed, fmt.Errorf("required: --instance-id")
	}
	if parsed.DiskPath == "" {
		parsed.DiskPath = "/"
	}
	return parsed, nil
}

func startHeartbeatSidecarProcess(r2Bucket string, instanceID int64, diskPath, phaseFile, fatalFile string) func() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start heartbeat sidecar: %v\n", err)
		return func() {}
	}

	cmd := exec.Command(exe,
		"heartbeat-sidecar",
		"--r2-bucket="+r2Bucket,
		fmt.Sprintf("--instance-id=%d", instanceID),
		"--disk-path="+diskPath,
		"--phase-file="+phaseFile,
		"--fatal-file="+fatalFile,
		fmt.Sprintf("--parent-pid=%d", os.Getpid()),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start heartbeat sidecar: %v\n", err)
		return func() {}
	}

	done := make(chan struct{})
	fatalAgentGo("heartbeat-sidecar-wait", func() {
		_ = cmd.Wait()
		close(done)
	})

	return func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
}

func writeLocalPhaseFile(path, phase string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(phase+"\n"), 0o644)
}

func readLocalPhaseFile(path string) string {
	return readLocalTextFile(path)
}

func readLocalTextFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func parentProcessAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	if os.Getppid() != pid {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
