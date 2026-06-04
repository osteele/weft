package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
)

type sshMockResponse struct {
	Contains string
	Stdout   string
	Stderr   string
	ExitCode int
}

// mockSSHCommands sets up SSH mocking for tests using the new SetRunner API.
// This doesn't spawn any processes - the mock returns results directly.
func mockSSHCommands(t *testing.T, responses []sshMockResponse) {
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		for _, resp := range responses {
			if resp.Contains == "" || strings.Contains(command, resp.Contains) {
				var err error
				if resp.ExitCode != 0 {
					err = fmt.Errorf("exit status %d", resp.ExitCode)
				}
				return resp.Stdout, resp.Stderr, err
			}
		}
		return "", "", nil
	})
	t.Cleanup(cleanup)
}

// mockSSHFunc creates an SSH mock from a simple handler function.
// The handler returns (stdout, stderr, exitCode).
func mockSSHFunc(t *testing.T, handler func(host, command string) (string, string, int)) {
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		stdout, stderr, exitCode := handler(host, command)
		var err error
		if exitCode != 0 {
			err = fmt.Errorf("exit status %d", exitCode)
		}
		return stdout, stderr, err
	})
	t.Cleanup(cleanup)
}

func mockQueueSourceSync(t *testing.T, sourceSHA256 string) {
	t.Helper()
	orig := queueSourceSync
	queueSourceSync = func(job *db.Job, timeout time.Duration) (string, error) {
		return sourceSHA256, nil
	}
	t.Cleanup(func() {
		queueSourceSync = orig
	})
}
