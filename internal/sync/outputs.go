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

// SyncOutputFilesBack rsyncs an explicit set of output files from a remote host
// back to a local directory. Paths are relative to remoteDir/localDir.
func SyncOutputFilesBack(host, remoteDir, localDir string, files []string, totalSizeMB, maxMB int) error {
	if maxMB > 0 && totalSizeMB > maxMB {
		return fmt.Errorf("output size %d MB exceeds auto-sync limit %d MB", totalSizeMB, maxMB)
	}
	if len(files) == 0 {
		return nil
	}
	args := BuildOutputFileSyncArgs(host, remoteDir, localDir)
	cmd := exec.Command("rsync", args...)
	cmd.Stdin = strings.NewReader(strings.Join(cleanOutputFileList(files), "\n") + "\n")
	return cmd.Run()
}

// BuildOutputSyncArgs constructs rsync arguments for pulling outputs from a remote host.
// Unlike source sync, this goes remote -> local with no --delete.
func BuildOutputSyncArgs(host, remoteDir, localDir, outputDir string) []string {
	dir := strings.TrimSuffix(outputDir, "/")
	src := ssh.RsyncTarget(host) + ":" + strings.TrimRight(remoteDir, "/") + "/" + dir + "/"
	dst := strings.TrimRight(localDir, "/") + "/" + dir + "/"
	return []string{"-az", "-e", ssh.BatchModeRsyncCommandForHost(host, rsyncConnectTimeout), src, dst}
}

// BuildOutputFileSyncArgs constructs rsync arguments for pulling a file list
// relative to remoteDir into localDir.
func BuildOutputFileSyncArgs(host, remoteDir, localDir string) []string {
	src := ssh.RsyncTarget(host) + ":" + strings.TrimRight(remoteDir, "/") + "/"
	dst := strings.TrimRight(localDir, "/") + "/"
	return []string{"-az", "--files-from=-", "-e", ssh.BatchModeRsyncCommandForHost(host, rsyncConnectTimeout), src, dst}
}

func cleanOutputFileList(files []string) []string {
	cleaned := make([]string, 0, len(files))
	for _, file := range files {
		file = strings.TrimSpace(strings.TrimPrefix(file, "/"))
		if file == "" || strings.HasPrefix(file, "../") || file == ".." || strings.Contains(file, "/../") {
			continue
		}
		cleaned = append(cleaned, file)
	}
	return cleaned
}
