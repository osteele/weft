package dataloc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// envAssignRe matches a leading shell environment assignment token like
// "FOO=bar" or "UV_INDEX_URL=https://...". Such tokens precede the actual
// program in a command and must be skipped when locating the exec target.
var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// CheckBareScriptExecutable fails fast when a command directly executes a local
// script file that lacks the executable (+x) bit. Running such a command bare
// (e.g. "scripts/run.py" rather than "python scripts/run.py") makes the shell
// exec the file as a program, which fails at runtime with exit code 126
// "Permission denied".
//
// The program being invoked is bare-exec only when the command's first token
// (after skipping leading KEY=value env assignments) resolves to an existing
// regular file under localDir. Interpreter-prefixed commands ("python foo.py",
// "uv run foo.py", "bash run.sh") have a first token that is not a local file
// and are left untouched.
//
// The check is deliberately conservative: it fires only when the file exists
// locally and has no execute bit set. A missing file is not blocked here (it is
// often a working-tree / sync nuance, not a defect), and a correctly chmod'd
// script passes through.
//
// localDir == "" means the job runs in the remote home with no local mapping,
// so there is nothing to stat; the check is a no-op in that case.
func CheckBareScriptExecutable(localDir, command string) error {
	if localDir == "" {
		return nil
	}

	token := firstExecToken(command)
	if token == "" {
		return nil
	}

	abs := token
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(localDir, token)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		// Not a local file (interpreter on $PATH) or missing — not our case.
		return nil
	}
	if info.Mode()&0o111 != 0 {
		// Already executable.
		return nil
	}

	return errors.New(bareExecMessage(localDir, token))
}

// firstExecToken returns the first program token of a command, skipping any
// leading KEY=value environment-assignment tokens. Returns "" if the command
// has no program token.
func firstExecToken(command string) string {
	for _, tok := range strings.Fields(command) {
		if envAssignRe.MatchString(tok) {
			continue
		}
		return tok
	}
	return ""
}

// bareExecMessage builds the actionable error for a non-executable bare-exec
// script. Python scripts get an interpreter suggestion (uv run when the project
// has a pyproject.toml, otherwise python); every script gets a chmod +x
// suggestion.
func bareExecMessage(localDir, token string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is not executable, but the command runs it directly.\n", token)
	b.WriteString("Either invoke it through an interpreter or make it executable:\n")

	if strings.HasSuffix(token, ".py") {
		if fileExists(filepath.Join(localDir, "pyproject.toml")) {
			fmt.Fprintf(&b, "  uv run %s    # use the project's pyproject.toml / uv environment\n", token)
		} else {
			fmt.Fprintf(&b, "  python %s\n", token)
		}
	}
	fmt.Fprintf(&b, "  chmod +x %s  # run it directly (requires a #! shebang line)", token)
	return b.String()
}

// fileExists reports whether path names an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
