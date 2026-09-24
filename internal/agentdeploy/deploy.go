package agentdeploy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/ssh"
)

const remoteBinDir = "~/.cache/weft/bin"

// EnsureAgentOptions tunes how EnsureAgentUpToDateWithOptions reports progress
// and records successful verification.
//   - Output receives raw subprocess stdout/stderr from the underlying builder
//     (rsync, flyctl, ssh build). Defaults to os.Stderr; pass io.Discard from
//     TUI contexts to avoid corrupting the screen.
//   - OnProgress receives coarse phase events ("starting fly builder",
//     "syncing source", "building", "downloading"). Use this to render a
//     status line.
//   - OnVerified receives the fingerprint after the installed binary has run
//     and matched the desired build.
//   - OnIndeterminate receives the local and remote identities when this
//     process cannot judge the deployed binary, so the caller can say why
//     nothing was deployed.
type EnsureAgentOptions struct {
	Output          io.Writer
	OnProgress      BuildProgressFunc
	OnVerified      func(version string)
	OnIndeterminate func(localVersion, remoteVersion string)
}

// agentDeployDecision is what a comparison of the local and remote agent
// identities settles.
type agentDeployDecision int

const (
	// agentDeployUpToDate: the host already runs this identity.
	agentDeployUpToDate agentDeployDecision = iota
	// agentDeployProceed: the local identity is authoritative and differs.
	agentDeployProceed
	// agentDeployIndeterminate: the identities are not comparable.
	agentDeployIndeterminate
)

// decideAgentDeploy compares the identity this process can compute against the
// one the host reports.
//
// A revision fallback cannot judge a source-hash deployment. The two name the
// same tree differently, so reading the disagreement as staleness overwrites a
// binary that was verified against its own source and re-execs the runner on
// every pass — observed on studio, where a source-hash deploy was replaced by
// a revision-identity rebuild seventy seconds later. Unknown is not stale: the
// deployed binary stays, and the process that can hash the source decides.
func decideAgentDeploy(localVer, remoteVer string) agentDeployDecision {
	if localVer == remoteVer {
		return agentDeployUpToDate
	}
	if AgentVersionKindOf(localVer) == AgentVersionRevisionFallback &&
		AgentVersionKindOf(remoteVer) == AgentVersionSourceHash {
		return agentDeployIndeterminate
	}
	return agentDeployProceed
}

// EnsureAgentUpToDate checks whether the remote agent is current, and if not,
// deploys a matching local build or falls back to a native build on the host.
// Returns true if a new binary was deployed. Subprocess output is sent to
// os.Stderr; for callers that render their own UI, use
// EnsureAgentUpToDateWithOptions.
func EnsureAgentUpToDate(host string, spec inventory.HostSpec) (bool, error) {
	return EnsureAgentUpToDateWithOptions(host, spec, EnsureAgentOptions{Output: os.Stderr})
}

// EnsureAgentUpToDateWithOptions is like EnsureAgentUpToDate but lets the
// caller redirect subprocess output and observe build phases. The active
// build phase for this host is cleared from the global registry on return so
// the TUI doesn't show a stale entry if the build errors out partway.
func EnsureAgentUpToDateWithOptions(host string, spec inventory.HostSpec, opts EnsureAgentOptions) (bool, error) {
	defer SetBuildPhase(host, "")
	output := opts.Output
	if output == nil {
		output = io.Discard
	}
	onProgress := opts.OnProgress
	if onProgress == nil {
		onProgress = func(string) {}
	}

	localVer, err := LocalAgentVersionForTarget(spec.OS, spec.Arch)
	if err != nil {
		return false, fmt.Errorf("local agent version: %w", err)
	}

	remoteVer, err := RemoteAgentVersion(host)
	if errors.Is(err, ErrAgentIncompatible) {
		// Binary exists but is broken (e.g. wrong arch or libc mismatch).
		// Redeploy using a build path that can produce a runnable binary.
		slog.Debug("agent incompatible, redeploying", "component", "agentdeploy", "host", host, "error", err)
	} else if err != nil {
		return false, fmt.Errorf("remote agent version on %s: %w", host, err)
	}

	switch decideAgentDeploy(localVer, remoteVer) {
	case agentDeployUpToDate:
		if opts.OnVerified != nil {
			opts.OnVerified(localVer)
		}
		return false, nil
	case agentDeployIndeterminate:
		slog.Warn("agent identity is not comparable; leaving the deployed binary in place",
			"component", "agentdeploy", "host", host,
			"local_version", localVer, "remote_version", remoteVer)
		if opts.OnIndeterminate != nil {
			opts.OnIndeterminate(localVer, remoteVer)
		}
		return false, nil
	}

	binaryPath, err := EnsureBuiltWithProgress(localVer, spec.OS, spec.Arch, output, onProgress)
	if errors.Is(err, ErrAgentNotAvailable) {
		// No pre-built binary — build natively on the remote host.
		onProgress("no compatible cached agent; building natively")
		if err := BuildOnHostWithProgress(host, localVer, spec.OS, spec.Arch, onProgress); err != nil {
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
		slog.Debug("cross-compiled agent incompatible, building natively", "component", "agentdeploy", "host", host, "error", err)
		onProgress("cached agent is not runnable there; building natively")
		if err := ensureCurrentBuildVersionMatchesInstalledIdentity(localVer, spec.OS, spec.Arch); err != nil {
			return false, fmt.Errorf(
				"cached agent for %s/%s is not runnable on %s; native rebuild refused: %w",
				spec.OS, spec.Arch, host, err)
		}
		if err := BuildOnHostWithProgress(host, localVer, spec.OS, spec.Arch, onProgress); err != nil {
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
	if opts.OnVerified != nil {
		opts.OnVerified(localVer)
	}

	// Send a restart command so the runner re-execs with the new binary,
	// preserving the tmux session and any running jobs.
	runner := queuerunner.NewRunner(host)
	if err := runner.SendRestartSignal(); err != nil {
		var qaErr *opsqueue.QueueAppendError
		if errors.As(err, &qaErr) && qaErr.IsConnectionError() {
			slog.Warn("host unreachable, cannot restart runner",
				"component", "agentdeploy", "host", host, "error", err)
		} else {
			slog.Warn("failed to send restart command, falling back to session kill",
				"component", "agentdeploy", "host", host, "error", err)
			if err := ssh.TmuxKillSession(host, runner.SessionName()); err != nil {
				slog.Warn("failed to kill runner session", "component", "agentdeploy", "host", host, "error", err)
			}
		}
	}

	return true, nil
}
