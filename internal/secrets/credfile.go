package secrets

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
)

// permissiveModeBits are the group and other permission bits. A file holding a
// credential should carry none of them.
const permissiveModeBits fs.FileMode = 0o077

var warnedPaths sync.Map

// ReadCredentialFile reads a file expected to hold secrets, warning once per
// path when its mode lets other accounts read it.
//
// The warning exists because a credential file's mode is invisible to everyone
// who is not looking for it: the value is read successfully, nothing fails, and
// the exposure is discovered only if someone happens to run ls. A weft
// notification webhook sat group- and world-readable on a shared host for
// months, on a machine with a second account, and was found by inspection
// rather than by anything weft said.
//
// A missing file is not an error here. These files are optional fallbacks for
// an environment variable, so absence is the common case and says nothing.
func ReadCredentialFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if mode := info.Mode(); mode&permissiveModeBits != 0 {
		if _, already := warnedPaths.LoadOrStore(path, struct{}{}); !already {
			slog.Warn("credential file is readable by other accounts",
				"component", "secrets",
				"path", path,
				"mode", fmt.Sprintf("%04o", mode.Perm()),
				"fix", fmt.Sprintf("chmod 600 %s", path))
		}
	}
	return os.ReadFile(path)
}
