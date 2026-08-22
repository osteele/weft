package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// SourceMarkerFile stores the rolling source snapshot hash in each synced
// working directory on the remote host. Kept for human-debugging and
// backward-compat; agents read the per-job marker file (see
// PerJobSourceMarkerFile) so peer-job syncs cannot invalidate an
// already-queued job's preflight check.
const SourceMarkerFile = ".weft-source.sha256"

// PerJobSourceMarkerFile returns the per-job source marker filename for jobID.
// Each queued job gets its own marker stamped at dispatch time, so concurrent
// jobs sharing a working dir don't race on the rolling marker.
func PerJobSourceMarkerFile(jobID int64) string {
	return fmt.Sprintf(".weft-source.%d.sha256", jobID)
}

// ComputeSourceSHA256 computes a deterministic SHA-256 fingerprint of the
// canonical tar stream for the source snapshot that SyncSources would send
// (same exclude rules). It does not hash the gzip representation.
func ComputeSourceSHA256(localDir string) (string, error) {
	return ComputeSourceSHA256ForCommands(localDir, nil)
}

func ComputeSourceSHA256ForCommands(localDir string, commands []string) (string, error) {
	roots, _, err := ResolveSourceRootsForCommands(localDir, commands)
	if err != nil {
		return "", err
	}
	if len(roots) > 1 {
		manifest, tmpPaths, err := BuildSourceManifestForInputsAndCommands(localDir, nil, commands)
		if err != nil {
			return "", err
		}
		removeFiles(tmpPaths)
		return manifest.Hash, nil
	}
	tarPath, sha256hex, err := CreateSourceTarball(localDir)
	if err != nil {
		return "", err
	}
	_ = os.Remove(tarPath)
	return sha256hex, nil
}

// WriteRemoteSourceMarker writes SourceMarkerFile into the remote working
// directory after a successful source sync.
func WriteRemoteSourceMarker(host, remoteDir, sha256hex string, timeout time.Duration) error {
	if strings.TrimSpace(sha256hex) == "" {
		return nil
	}
	escapedHash := ssh.EscapeForSingleQuotes(sha256hex)
	trimmedDir := strings.TrimRight(remoteDir, "/")
	markerPath := fmt.Sprintf("%s/%s", trimmedDir, SourceMarkerFile)
	cmd := fmt.Sprintf("mkdir -p %s && printf '%%s\\n' '%s' > %s", trimmedDir, escapedHash, markerPath)
	_, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return fmt.Errorf("write source marker: %s: %w", strings.TrimSpace(stderr), err)
	}
	return nil
}

// WriteRemoteSourceMarkerForJob writes the per-job source marker into the
// remote working directory. Called once per queued job at dispatch time; the
// agent reads this file (not the rolling marker) so peer-job syncs to the
// same working dir don't invalidate this job's preflight check.
func WriteRemoteSourceMarkerForJob(host, remoteDir string, jobID int64, sha256hex string, timeout time.Duration) error {
	if strings.TrimSpace(sha256hex) == "" {
		return nil
	}
	escapedHash := ssh.EscapeForSingleQuotes(sha256hex)
	trimmedDir := strings.TrimRight(remoteDir, "/")
	markerPath := fmt.Sprintf("%s/%s", trimmedDir, PerJobSourceMarkerFile(jobID))
	cmd := fmt.Sprintf("mkdir -p %s && printf '%%s\\n' '%s' > %s", trimmedDir, escapedHash, markerPath)
	_, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return fmt.Errorf("write per-job source marker: %s: %w", strings.TrimSpace(stderr), err)
	}
	return nil
}

// RemovePerJobSourceMarker deletes the per-job source marker file from the
// remote working directory. Called on job final state (completion, failure,
// kill, cancel) to keep the working dir tidy.
func RemovePerJobSourceMarker(host, remoteDir string, jobID int64, timeout time.Duration) error {
	trimmedDir := strings.TrimRight(remoteDir, "/")
	markerPath := fmt.Sprintf("%s/%s", trimmedDir, PerJobSourceMarkerFile(jobID))
	cmd := fmt.Sprintf("rm -f %s", markerPath)
	_, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return fmt.Errorf("remove per-job source marker: %s: %w", strings.TrimSpace(stderr), err)
	}
	return nil
}

// ReadSourceMarker reads SourceMarkerFile from a local working directory.
func ReadSourceMarker(localDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(localDir, SourceMarkerFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// ReadSourceMarkerForJob reads the per-job source marker file from localDir,
// falling back to the rolling SourceMarkerFile when the per-job file is
// absent. The fallback keeps already-queued jobs from older runner versions
// working during a rollout.
func ReadSourceMarkerForJob(localDir string, jobID int64) (string, error) {
	data, err := os.ReadFile(filepath.Join(localDir, PerJobSourceMarkerFile(jobID)))
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	return ReadSourceMarker(localDir)
}
