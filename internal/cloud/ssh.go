package cloud

import "os/exec"

// SSHRun executes a command on a remote host via SSH.
func SSHRun(target string, sshOpts []string, command string) (string, error) {
	args := append([]string{}, sshOpts...)
	args = append(args, target, command)
	cmd := exec.Command("ssh", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
