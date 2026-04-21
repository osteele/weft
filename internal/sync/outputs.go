package sync

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/osteele/weft/internal/ssh"
)

// SyncOutputsBack rsyncs output directories from a remote host back to a local directory.
// Only syncs if total size of the provided files is within maxMB (0 = no limit).
// Uses rsync -az (no --delete) to pull from remote to local.
func SyncOutputsBack(host, remoteDir, localDir string, dirs []string, totalSizeMB, maxMB int) error {
	if maxMB > 0 && totalSizeMB > maxMB {
		return fmt.Errorf("output size %d MB exceeds auto-sync limit %d MB", totalSizeMB, maxMB)
	}

	for _, dir := range dirs {
		args := BuildOutputSyncArgs(host, remoteDir, localDir, dir)
		cmd := exec.Command("rsync", args...)
		if err := cmd.Run(); err != nil {
			// Non-fatal: the directory may not exist on the remote
			continue
		}
	}
	return nil
}

// BuildOutputSyncArgs constructs rsync arguments for pulling outputs from a remote host.
// Unlike source sync, this goes remote -> local with no --delete.
func BuildOutputSyncArgs(host, remoteDir, localDir, outputDir string) []string {
	dir := strings.TrimSuffix(outputDir, "/")
	src := host + ":" + strings.TrimRight(remoteDir, "/") + "/" + dir + "/"
	dst := strings.TrimRight(localDir, "/") + "/" + dir + "/"
	return []string{"-az", "-e", ssh.BatchModeRsyncCommand(rsyncConnectTimeout), src, dst}
}
