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

// BatchModeRsyncCommandForHost returns an rsync -e command that uses the same
// configured identity isolation as ssh.Run for the given host.
func BatchModeRsyncCommandForHost(host string, connectTimeout time.Duration, extraOpts ...string) string {
	args := append(BatchModeArgs(connectTimeout, extraOpts...), identityArgs(host)...)
	return "ssh " + shellJoin(args)
}

// RsyncTarget returns the remote target prefix for rsync destinations/sources.
// It applies hosts.<name>.ssh_user so rsync and ssh.Run operate in the same
// remote account and home directory.
func RsyncTarget(host string) string {
	return hostTarget(host)
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`!#&;<>*?()[]{}|") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
