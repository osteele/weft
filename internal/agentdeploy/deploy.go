package agentdeploy

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
)

const remoteBinDir = "~/.cache/weft/bin"

// EnsureAgentUpToDate checks whether the remote agent is current, and if not,
// cross-compiles and deploys it. Returns true if a new binary was deployed.
func EnsureAgentUpToDate(host string, spec inventory.HostSpec) (bool, error) {
	localVer, err := LocalAgentVersion()
	if err != nil {
		return false, fmt.Errorf("local agent version: %w", err)
	}

	remoteVer, err := RemoteAgentVersion(host)
	if err != nil {
		return false, fmt.Errorf("remote agent version on %s: %w", host, err)
	}

	if localVer == remoteVer {
		return false, nil
	}

	binaryPath, err := EnsureBuilt(localVer, spec.OS, spec.Arch)
	if err != nil {
		return false, err
	}

	// Ensure remote bin directory exists
	mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteBinDir)
	if _, stderr, err := ssh.Run(host, mkdirCmd); err != nil {
		return false, fmt.Errorf("create remote bin dir: %s", strings.TrimSpace(stderr))
	}

	// Deploy via temp file + atomic rename. On Linux, you can't overwrite a
	// running binary, but rename(2) atomically replaces the directory entry
	// while the old inode remains open for the running process. When the runner
	// receives a restart command and calls syscall.Exec, it picks up the new binary.
	tmpPath := remoteAgentPath + ".tmp"
	if err := ssh.CopyTo(binaryPath, host, tmpPath); err != nil {
		return false, fmt.Errorf("deploy agent to %s: %w", host, err)
	}

	installCmd := fmt.Sprintf("chmod +x %s && mv %s %s", tmpPath, tmpPath, remoteAgentPath)
	if _, stderr, err := ssh.Run(host, installCmd); err != nil {
		return false, fmt.Errorf("install agent: %s", strings.TrimSpace(stderr))
	}

	return true, nil
}
