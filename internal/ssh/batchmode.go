package ssh

import (
	"fmt"
	"strings"
	"time"
)

// BatchModeArgs returns ssh CLI args that disable all interactive prompts
// (host-key confirmation, password, passphrase) via BatchMode=yes and enforce
// a connection timeout. Without BatchMode, ssh bypasses its piped stdin and
// opens /dev/tty directly to prompt — corrupting any TUI running in the same
// terminal and blocking on user input that never arrives.
//
// extraOpts are additional values for -o flags (e.g. "ConnectionAttempts=1",
// "ServerAliveInterval=15"), appended after BatchMode and ConnectTimeout.
// Callers append any remaining exec args (host, remote command) after.
func BatchModeArgs(connectTimeout time.Duration, extraOpts ...string) []string {
	secs := int(connectTimeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	args := []string{"-o", "BatchMode=yes", "-o", fmt.Sprintf("ConnectTimeout=%d", secs)}
	for _, o := range extraOpts {
		args = append(args, "-o", o)
	}
	return args
}

// BatchModeRsyncCommand returns an ssh command string suitable for rsync's -e
// flag, with the same non-interactive behavior as BatchModeArgs.
func BatchModeRsyncCommand(connectTimeout time.Duration, extraOpts ...string) string {
	return "ssh " + strings.Join(BatchModeArgs(connectTimeout, extraOpts...), " ")
}
