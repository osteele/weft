package dataloc

import (
	"os/exec"
	"strings"
	"testing"
)

// TestScanCommandsArePOSIXShell guards the shell dialect these commands are
// executed under. defaultHostCommandRunner runs them with `/bin/sh -lc`, and on
// the Debian family /bin/sh is dash, which has no arrays. The commands used
// `_dirs=()` and `${_dirs[@]}` and failed there with
// `Syntax error: "(" unexpected`, invisibly on macOS where /bin/sh is bash.
func TestScanCommandsArePOSIXShell(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash not installed; /bin/sh here is bash and accepts bashisms")
	}

	for name, command := range map[string]string{
		"corpusScanCommand":  corpusScanCommand(),
		"hfCacheScanCommand": hfCacheScanCommand(),
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(dash, "-n")
			cmd.Stdin = strings.NewReader(command)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("not valid POSIX shell: %v\n%s", err, out)
			}
		})
	}
}
