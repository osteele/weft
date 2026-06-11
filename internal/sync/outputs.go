package sync

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/osteele/weft/internal/ssh"
)

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
