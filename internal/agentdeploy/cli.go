package agentdeploy

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
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
	out, stderr, err := ssh.Run(host, fmt.Sprintf(
		"if [ -x %s ]; then %s version 2>/dev/null || echo UNRUNNABLE; else echo ABSENT; fi",
		RemoteCLIPath, RemoteCLIPath))
	if err != nil {
		return "", fmt.Errorf("query CLI version on %s: %w\n%s", host, err, strings.TrimSpace(stderr))
	}
	v := strings.TrimSpace(out)
	switch v {
	case "ABSENT":
		return "", nil
	case "UNRUNNABLE":
		// Present but does not execute — wrong architecture, most likely.
		// Treated as needing replacement rather than as a version.
		return "UNRUNNABLE", nil
	}
	return v, nil
}

// EnsureCLIOnHost installs or updates the weft CLI on a host, returning true
// when it changed anything.
//
// The CLI is needed on any host acting as an edge: `weft edge key mint` runs
// there by design, because the private key must be generated where it will be
// used and never travel. A host with only the agent binary cannot run it.
func EnsureCLIOnHost(host string, spec inventory.HostSpec) (bool, error) {
	localVer, err := LocalCLIVersion()
	if err != nil {
		return false, err
	}
	remoteVer, err := RemoteCLIVersion(host)
	if err != nil {
		return false, err
	}
	if remoteVer != "" && remoteVer == localVer {
		return false, nil
	}

	binary, err := cliBinaryFor(spec)
	if err != nil {
		return false, err
	}

	if _, stderr, err := ssh.Run(host, "mkdir -p "+RemoteCLIDir); err != nil {
		return false, fmt.Errorf("create %s on %s: %s", RemoteCLIDir, host, strings.TrimSpace(stderr))
	}

	// Temp file then rename, so a reader never sees a partially copied binary
	// and a failed copy does not leave the host without a working CLI.
	tmp := RemoteCLIPath + ".tmp"
	if err := ssh.CopyTo(binary, host, tmp); err != nil {
		return false, fmt.Errorf("copy CLI to %s: %w", host, err)
	}
	if _, stderr, err := ssh.Run(host, fmt.Sprintf("chmod +x %s && mv %s %s", tmp, tmp, RemoteCLIPath)); err != nil {
		return false, fmt.Errorf("install CLI on %s: %s", host, strings.TrimSpace(stderr))
	}

	// Verify it runs there rather than assuming a successful copy means a
	// usable binary; a wrong-architecture copy installs fine and fails later.
	deployed, err := RemoteCLIVersion(host)
	if err != nil {
		return true, err
	}
	if deployed == "UNRUNNABLE" {
		return true, fmt.Errorf(
			"installed CLI on %s does not execute there; it was built for %s/%s",
			host, spec.OS, spec.Arch)
	}
	return true, nil
}

// LocalCLIVersion is the version of the running binary.
func LocalCLIVersion() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running binary: %w", err)
	}
	out, err := runCommandCapture("", nil, self, "version")
	if err != nil {
		return "", fmt.Errorf("read local CLI version: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// cliBinaryFor returns a path to a CLI binary runnable on the target.
//
// When the target matches this machine, the running binary is itself the
// right artifact — which keeps the deployed CLI byte-identical to the one that
// deployed it, with no second build to drift. Cross-platform deployment needs
// a build step that does not exist yet and fails loudly rather than shipping
// something that will not execute.
func cliBinaryFor(spec inventory.HostSpec) (string, error) {
	if spec.OS == runtime.GOOS && spec.Arch == runtime.GOARCH {
		return os.Executable()
	}
	return "", fmt.Errorf(
		"cannot install the weft CLI on a %s/%s host from a %s/%s machine: "+
			"cross-platform CLI builds are not implemented.\n"+
			"Build weft on that host from source and place it at %s",
		spec.OS, spec.Arch, runtime.GOOS, runtime.GOARCH, RemoteCLIPath)
}
