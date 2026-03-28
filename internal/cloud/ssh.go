package cloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/osteele/weft/internal/retry"
)

// SSHRun executes a command on a remote host via SSH.
func SSHRun(target string, sshOpts []string, command string) (string, error) {
	args := append([]string{}, sshOpts...)
	args = append(args, target, command)
	cmd := exec.Command("ssh", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// InstanceSSHArgs returns the SSH arguments needed to connect to a cloud instance
// (port, host key checks disabled, log level). Does not include the target or command.
func InstanceSSHArgs(inst *Instance) []string {
	return []string{
		"-p", fmt.Sprintf("%d", inst.SSHPort),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
}

// InstanceSSHTarget returns the SSH target string (user@host) for a cloud instance.
func InstanceSSHTarget(inst *Instance) string {
	return fmt.Sprintf("root@%s", inst.SSHHost)
}

// RunOnInstance executes a command on a cloud instance via SSH using its
// provider-supplied SSH details.
func RunOnInstance(inst *Instance, command string, timeout time.Duration) (string, error) {
	if inst.SSHHost == "" {
		return "", fmt.Errorf("instance %s has no SSH host", inst.ProviderID)
	}
	return SSHRunWithRetry(InstanceSSHTarget(inst), InstanceSSHArgs(inst), command, timeout)
}

// SSHRunWithRetry executes a command on a remote host via SSH, retrying on
// connection failures (exit code 255) until timeout. Useful for freshly
// provisioned instances where sshd may not be ready immediately.
func SSHRunWithRetry(target string, sshOpts []string, command string, timeout time.Duration) (string, error) {
	return retry.DoVal(context.Background(), retry.ConstantWithDeadline(5*time.Second, timeout),
		func() (string, error) {
			return SSHRun(target, sshOpts, command)
		},
		retry.WithRetryIf(func(err error) bool {
			var exitErr *exec.ExitError
			return errors.As(err, &exitErr) && exitErr.ExitCode() == 255
		}),
		retry.WithOnRetry(func(attempt int, err error, delay time.Duration) {
			slog.Debug("SSH connection failed, retrying", "component", "cloud", "retry_in", delay)
		}),
	)
}
