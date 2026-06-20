package agentdeploy

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/osteele/weft/internal/sshaudit"
)

// DeployToSSH deploys a pre-built agent binary to a remote host via scp.
// sshTarget is "user@host", sshOpts are extra SSH flags (e.g., -p PORT),
// remotePath is where to install the binary (e.g., /usr/local/bin/weft-agent).
func DeployToSSH(localBinaryPath, sshTarget string, sshOpts []string, remotePath string) error {
	// Build scp args with SSH options
	tmpPath := remotePath + ".tmp"

	// scp -O forces legacy protocol (compatible with all sshd versions)
	scpArgs := []string{"-O"}
	if len(sshOpts) > 0 {
		// Convert SSH opts to scp-compatible form (-o and -P flags)
		for i := 0; i < len(sshOpts); i++ {
			switch sshOpts[i] {
			case "-p":
				if i+1 < len(sshOpts) {
					scpArgs = append(scpArgs, "-P", sshOpts[i+1])
					i++
				}
			case "-o":
				if i+1 < len(sshOpts) {
					scpArgs = append(scpArgs, "-o", sshOpts[i+1])
					i++
				}
			}
		}
	}
	scpArgs = append(scpArgs, localBinaryPath, sshTarget+":"+tmpPath)

	sshaudit.Log(sshaudit.KindSCP, sshTarget, 0)
	cmd := exec.Command("scp", scpArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scp agent to %s: %w\n%s", sshTarget, err, strings.TrimSpace(string(out)))
	}

	// chmod + atomic rename via SSH
	installCmd := fmt.Sprintf("chmod +x %s && mv %s %s", tmpPath, tmpPath, remotePath)
	sshArgs := append([]string{}, sshOpts...)
	sshArgs = append(sshArgs, sshTarget, installCmd)
	sshaudit.Log(sshaudit.KindCommand, sshTarget, 0)
	cmd = exec.Command("ssh", sshArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install agent on %s: %w\n%s", sshTarget, err, strings.TrimSpace(string(out)))
	}

	return nil
}
