package cloud

import (
	"errors"
	"log"
	"os/exec"
	"time"
)

// SSHRun executes a command on a remote host via SSH.
func SSHRun(target string, sshOpts []string, command string) (string, error) {
	args := append([]string{}, sshOpts...)
	args = append(args, target, command)
	cmd := exec.Command("ssh", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// SSHRunWithRetry executes a command on a remote host via SSH, retrying on
// connection failures (exit code 255) until timeout. Useful for freshly
// provisioned instances where sshd may not be ready immediately.
func SSHRunWithRetry(target string, sshOpts []string, command string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	poll := 5 * time.Second

	for {
		out, err := SSHRun(target, sshOpts, command)
		if err == nil {
			return out, nil
		}
		// Only retry on SSH connection failures (exit code 255)
		var exitErr *exec.ExitError
		isExitErr := errors.As(err, &exitErr)
		if isExitErr && exitErr.ExitCode() == 255 && time.Now().Before(deadline) {
			log.Printf("SSH connection failed (exit 255, output=%q), retrying in %v (deadline in %v)",
				out, poll, time.Until(deadline).Round(time.Second))
			time.Sleep(poll)
			continue
		}
		if isExitErr {
			log.Printf("SSH failed with exit code %d (not retrying): %s", exitErr.ExitCode(), out)
		} else {
			log.Printf("SSH failed with non-exit error (not retrying): %v", err)
		}
		return out, err
	}
}
