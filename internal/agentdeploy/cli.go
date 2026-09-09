package agentdeploy

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/inventory"
)

// RemoteCLIDir is where the weft CLI is installed on a host.
//
// This is deliberately not the agent's ~/.cache/weft/bin. The agent is a
// managed artifact weft replaces at will; the CLI is a command a person or an
// agent session invokes by name, so it belongs on PATH in the conventional
// user-local location — the same one `weft host setup` already uses for rclone.
const RemoteCLIDir = "~/.local/bin"

// RemoteCLIPath is the installed CLI.
const RemoteCLIPath = RemoteCLIDir + "/weft"

// RemoteCLIVersion reports the CLI version installed on a host, or "" when no
// CLI is installed.
//
// An absent binary is reported as "" with no error, because a host that has
// never had the CLI is a normal state rather than a failure. Any other error
// means the host could not be asked, which callers must not read as absence.
func RemoteCLIVersion(host string) (string, error) {
	out, stderr, err := sshRunFunc(host, fmt.Sprintf(
		"if [ -x %s ]; then %s version 2>/dev/null || echo UNRUNNABLE; else echo ABSENT; fi",
		RemoteCLIPath, RemoteCLIPath))
	if err != nil {
		return "", fmt.Errorf("query CLI version on %s: %w\n%s", host, err, strings.TrimSpace(stderr))
	}
	value := strings.TrimSpace(out)
	switch value {
	case "ABSENT":
		return "", nil
	case "UNRUNNABLE":
		return "UNRUNNABLE", nil
	}
	version, ok := strings.CutPrefix(value, "weft ")
	if !ok || strings.TrimSpace(version) == "" {
		return "", fmt.Errorf("query CLI version on %s: unexpected output %q", host, value)
	}
	return strings.TrimSpace(version), nil
}

// EnsureCLIOnHost installs or updates the weft CLI on a host, returning true
// when it changed anything.
//
// The CLI is needed on any host acting as an edge: `weft edge key mint` runs
// there by design, because the private key must be generated where it will be
// used and never travel. A host with only the agent binary cannot run it.
func EnsureCLIOnHost(host string, spec inventory.HostSpec) (bool, error) {
	localVer, err := localCLIVersionFunc(spec.OS, spec.Arch)
	if err != nil {
		return false, err
	}
	remoteVer, err := remoteCLIVersionFunc(host)
	if err != nil {
		return false, err
	}
	if remoteVer == localVer {
		return false, nil
	}

	if err := buildCLIOnHostFunc(host, localVer, spec.OS, spec.Arch); err != nil {
		return false, err
	}

	deployed, err := remoteCLIVersionFunc(host)
	if err != nil {
		return true, err
	}
	if deployed != localVer {
		return true, fmt.Errorf(
			"deployed CLI version mismatch on %s: want %s, got %q",
			host, localVer, deployed)
	}
	return true, nil
}

var (
	localCLIVersionFunc  = LocalCLIVersion
	remoteCLIVersionFunc = RemoteCLIVersion
	buildCLIOnHostFunc   = BuildCLIOnHost
)

// LocalCLIVersion fingerprints the Go source that builds the CLI. The
// fingerprint includes uncommitted working-tree changes, matching the source
// snapshot BuildCLIOnHost sends to the target.
func LocalCLIVersion(goos, goarch string) (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return localCLISourceVersion(root, goos, goarch)
}
