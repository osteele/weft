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
	src := ssh.RsyncTarget(host) + ":" + remoteRsyncPath(remoteDir) + "/"
	dst := strings.TrimRight(localDir, "/") + "/"
	return []string{"-az", "--files-from=-", "-e", ssh.BatchModeRsyncCommandForHost(host, rsyncConnectTimeout), src, dst}
}

// remoteRsyncPath normalizes a remote directory for use after "host:" in an
// rsync target. rsync does not run the path through a login shell, so a
// leading "~/" is taken literally (rsync resolves "host:relpath" relative to
// the remote login home already). Strip the tilde prefix so "~/code/x"
// becomes the home-relative "code/x" rather than a literal "~" directory.
func remoteRsyncPath(remoteDir string) string {
	remoteDir = strings.TrimRight(remoteDir, "/")
	switch {
	case remoteDir == "~":
		return "."
	case strings.HasPrefix(remoteDir, "~/"):
		return strings.TrimPrefix(remoteDir, "~/")
	default:
		return remoteDir
	}
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
