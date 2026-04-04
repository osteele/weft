package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// SourceMarkerFile stores the expected source snapshot hash in each synced
// working directory on the remote host.
const SourceMarkerFile = ".weft-source.sha256"

// ComputeSourceSHA256 computes a deterministic SHA-256 fingerprint for the
// source snapshot that SyncSources would send (same exclude rules).
func ComputeSourceSHA256(localDir string) (string, error) {
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

// ReadSourceMarker reads SourceMarkerFile from a local working directory.
func ReadSourceMarker(localDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(localDir, SourceMarkerFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
