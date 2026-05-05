package cloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/retry"
)

var sshIdentity struct {
	mu   sync.RWMutex
	file string
}

// SetSSHIdentityFile configures the private key used for cloud SSH.
func SetSSHIdentityFile(path string) {
	sshIdentity.mu.Lock()
	defer sshIdentity.mu.Unlock()
	sshIdentity.file = strings.TrimSpace(path)
}

// SSHIdentityFile returns the configured cloud SSH private key path.
func SSHIdentityFile() string {
	sshIdentity.mu.RLock()
	defer sshIdentity.mu.RUnlock()
	return sshIdentity.file
}

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
	args := []string{
		"-p", fmt.Sprintf("%d", inst.SSHPort),
		// Pass -F /dev/null so the user's ~/.ssh/config doesn't smuggle in
		// extra IdentityFile entries (a Host ssh*.vast.ai stanza is common
		// and would override IdentitiesOnly=yes by adding the user's
		// hardware-backed key, prompting for interactive auth instead of
		// using the dedicated weft cloud key).
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "IdentitiesOnly=yes",
		// Belt-and-braces: also disable any running ssh-agent so we
		// don't fall back to whatever keys it's offering.
		"-o", "IdentityAgent=none",
	}
	if identity := SSHIdentityFile(); identity != "" {
		args = append(args, "-i", identity)
	}
	return args
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

// CommandString returns a shell-ready command string for display.
func CommandString(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = shellQuote(arg)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\n\"'`$\\~") {
		return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
	}
	return s
}
