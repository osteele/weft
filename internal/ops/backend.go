package ops

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
)

func ResolveBackend(host string, timeout time.Duration) (string, error) {
	cfg, _ := config.Load()
	if cfg != nil {
		if backend := normalizeBackend(cfg.HostBackend(host)); backend != "" {
			return backend, nil
		}
	}
	return probeBackend(host, timeout)
}

func normalizeBackend(backend string) string {
	backend = strings.ToLower(strings.TrimSpace(backend))
	switch backend {
	case db.BackendQueueRunner, db.BackendSlurm, db.BackendVastai:
		return backend
	default:
		return ""
	}
}

func probeBackend(host string, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	queueRunnerInstalled, err := hasQueueRunner(host, timeout)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return "", err
		}
	}
	if queueRunnerInstalled {
		return db.BackendQueueRunner, nil
	}
	slurmAvailable, err := hasSlurm(host, timeout)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return "", err
		}
	}
	if slurmAvailable {
		return db.BackendSlurm, nil
	}
	return db.BackendQueueRunner, nil
}

func hasQueueRunner(host string, timeout time.Duration) (bool, error) {
	cmd := "test -x ~/.cache/weft/bin/weft-agent"
	_, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return false, fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
		}
		return false, nil
	}
	return true, nil
}

func hasSlurm(host string, timeout time.Duration) (bool, error) {
	cmd := "command -v sbatch >/dev/null 2>&1 && command -v squeue >/dev/null 2>&1 && command -v sacct >/dev/null 2>&1"
	_, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return false, fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
		}
		return false, nil
	}
	return true, nil
}
