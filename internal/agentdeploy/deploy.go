package agentdeploy

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/queuerunner"
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
	if errors.Is(err, ErrAgentIncompatible) {
		// Binary exists but is broken (e.g. wrong arch). Deploy the correct build.
		log.Printf("agent on %s is incompatible, redeploying: %v", host, err)
	} else if err != nil {
		return false, fmt.Errorf("remote agent version on %s: %w", host, err)
	}

	if localVer == remoteVer {
		return false, nil
	}

	binaryPath, err := EnsureBuilt(localVer, spec.OS, spec.Arch, "")
	if errors.Is(err, ErrAgentNotAvailable) {
		// No pre-built binary — build natively on the remote host.
		if err := BuildOnHost(host, localVer); err != nil {
			return false, fmt.Errorf("native build on %s: %w", host, err)
		}
		// Binary was built directly into place; skip SCP.
	} else if err != nil {
		return false, err
	} else {
		// Ensure remote bin directory exists.
		mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteBinDir)
		if _, stderr, err := ssh.Run(host, mkdirCmd); err != nil {
			return false, fmt.Errorf("create remote bin dir: %s", strings.TrimSpace(stderr))
		}

		// Deploy via temp file + atomic rename to avoid partial writes.
		tmpPath := remoteAgentPath + ".tmp"
		if err := ssh.CopyTo(binaryPath, host, tmpPath); err != nil {
			return false, fmt.Errorf("deploy agent to %s: %w", host, err)
		}

		installCmd := fmt.Sprintf("chmod +x %s && mv %s %s", tmpPath, tmpPath, remoteAgentPath)
		if _, stderr, err := ssh.Run(host, installCmd); err != nil {
			return false, fmt.Errorf("install agent: %s", strings.TrimSpace(stderr))
		}
	}

	// Verify the newly deployed binary actually runs.
	deployedVer, err := RemoteAgentVersion(host)
	if errors.Is(err, ErrAgentIncompatible) {
		// Cross-compiled binary doesn't run (e.g. GLIBC mismatch). Fall back to native build.
		log.Printf("cross-compiled agent incompatible on %s, building natively: %v", host, err)
		if err := BuildOnHost(host, localVer); err != nil {
			return false, fmt.Errorf("native build on %s: %w", host, err)
		}
		deployedVer, err = RemoteAgentVersion(host)
	}
	if err != nil {
		return false, fmt.Errorf("deployed agent is not runnable on %s: %w", host, err)
	}
	if deployedVer != localVer {
		return false, fmt.Errorf("deployed agent version mismatch on %s: want %s, got %q (binary may be incompatible)", host, localVer, deployedVer)
	}

	// Kill the runner tmux session so EnsureRunnerStarted recreates it with the new binary.
	if err := ssh.TmuxKillSession(host, queuerunner.RunnerSessionName()); err != nil {
		log.Printf("warning: failed to kill runner session on %s: %v", host, err)
	}

	return true, nil
}
