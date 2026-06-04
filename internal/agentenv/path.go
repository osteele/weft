package agentenv

import (
	"os"
	"strings"
)

var staticToolDirs = []string{
	"/opt/homebrew/bin",
	"/opt/homebrew/sbin",
	"/usr/local/bin",
	"/usr/local/sbin",
}

// ToolPathDirs returns the PATH entries that an on-prem agent should see.
// The first entries are per-user install locations; the static entries cover
// package-manager locations that are often absent from non-login SSH sessions.
func ToolPathDirs(home string) []string {
	dirs := []string{}
	if home != "" {
		dirs = append(dirs, home+"/.cache/weft/bin", home+"/.local/bin", home+"/bin")
	}
	dirs = append(dirs, staticToolDirs...)
	return dirs
}

func ShellPathAssignment() string {
	return `PATH="$HOME/.cache/weft/bin:$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/local/sbin:$PATH"`
}

func ShellExportPath() string {
	return `export ` + ShellPathAssignment()
}

func ShellPrefix(command string) string {
	return ShellExportPath() + "; " + command
}

func EnsureToolPath() {
	home, _ := os.UserHomeDir()
	prefixes := ToolPathDirs(home)

	path := os.Getenv("PATH")
	parts := strings.Split(path, string(os.PathListSeparator))
	have := make(map[string]bool, len(parts))
	for _, part := range parts {
		have[part] = true
	}

	added := make([]string, 0, len(prefixes)+len(parts))
	for _, prefix := range prefixes {
		if prefix != "" && !have[prefix] {
			added = append(added, prefix)
			have[prefix] = true
		}
	}
	added = append(added, parts...)
	os.Setenv("PATH", strings.Join(added, string(os.PathListSeparator)))
}
