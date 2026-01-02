package ops

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/osteele/remote-jobs/internal/ssh"
)

type sshMockResponse struct {
	Contains string
	Stdout   string
	Stderr   string
	ExitCode int
}

func mockSSHCommands(t *testing.T, responses []sshMockResponse) {
	handler := func(host, command string) (string, string, int) {
		for _, resp := range responses {
			if resp.Contains == "" || strings.Contains(command, resp.Contains) {
				return resp.Stdout, resp.Stderr, resp.ExitCode
			}
		}
		return "", "", 0
	}

	cleanup := ssh.SetExecCommand(mockSSHExecCommand(handler))
	t.Cleanup(cleanup)
}

func mockSSHExecCommand(handler func(host, command string) (string, string, int)) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		if name != "ssh" {
			return exec.Command(name, args...)
		}
		host, command := parseHostAndCommand(args)
		stdout, stderr, exitCode := handler(host, command)
		return exec.Command("sh", "-c", buildMockCommand(stdout, stderr, exitCode))
	}
}

func parseHostAndCommand(args []string) (string, string) {
	if len(args) >= 2 {
		return args[len(args)-2], args[len(args)-1]
	}
	if len(args) == 1 {
		return "", args[0]
	}
	return "", ""
}

func buildMockCommand(stdout, stderr string, exitCode int) string {
	return fmt.Sprintf("printf '%%s' %s; >&2 printf '%%s' %s; exit %d",
		singleQuote(stdout), singleQuote(stderr), exitCode)
}

func singleQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
