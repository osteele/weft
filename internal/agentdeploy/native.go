package agentdeploy

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

const remoteAgentBuildDir = "~/.cache/weft/agent-build"
const remoteGoDir = "~/.local/go"

// BuildOnHost builds the agent natively on a remote inventory host.
// It rsyncs the source tree, ensures Go is installed, and compiles the agent
// binary directly into the remote bin directory.
func BuildOnHost(host, version string) error {
	root, err := RepoRoot()
	if err != nil {
		return fmt.Errorf("locate repo root: %w", err)
	}

	log.Printf("syncing sources to %s:%s...", host, remoteAgentBuildDir)
	if err := rsyncSourcesToHost(root, host); err != nil {
		return fmt.Errorf("rsync sources: %w", err)
	}

	log.Printf("ensuring Go is available on %s...", host)
	gobin, err := ensureGoOnHost(host)
	if err != nil {
		return fmt.Errorf("ensure Go: %w", err)
	}

	log.Printf("building agent on %s (version %s)...", host, version)
	buildCmd := fmt.Sprintf(
		`mkdir -p %s && cd %s && %s build -ldflags "-X main.version=%s" -o %s ./cmd/agent`,
		remoteBinDir, remoteAgentBuildDir, gobin, version, remoteAgentPath,
	)
	_, stderr, err := ssh.RunWithTimeout(host, buildCmd, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("build: %s", strings.TrimSpace(stderr))
	}

	return nil
}

// rsyncSourcesToHost syncs the weft source tree to the remote host's build
// directory, using the same exclusions as build-agent-on-fly.sh.
func rsyncSourcesToHost(localRoot, host string) error {
	args := []string{
		"-az", "--delete",
		"--exclude=.git/", "--exclude=.jj/", "--exclude=.claude/",
		"--exclude=.gocache/", "--exclude=.gomodcache/", "--exclude=.cache/",
		"--exclude=.bench-*-gocache/", "--exclude=.bench-*-gomodcache/",
		"--exclude=testdata/", "--exclude=dist/",
		"--exclude=internal/agentdeploy/binaries/weft-agent-*",
		"--exclude=internal/agentdeploy/binaries/VERSION",
		"--exclude=weft", "--exclude=placement.test",
		localRoot + "/",
		host + ":" + remoteAgentBuildDir + "/",
	}
	cmd := exec.Command("rsync", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ensureGoOnHost checks whether Go is available on the remote host, installing
// it under ~/.local/go if needed. Returns the path to the go binary.
func ensureGoOnHost(host string) (string, error) {
	// Check common locations: PATH, then ~/.local/go/bin/go.
	checkCmd := fmt.Sprintf(
		`if command -v go >/dev/null 2>&1; then command -v go; elif [ -x %s/bin/go ]; then echo %s/bin/go; fi`,
		remoteGoDir, remoteGoDir,
	)
	stdout, _, err := ssh.Run(host, checkCmd)
	gobin := strings.TrimSpace(stdout)

	// Verify the binary actually runs.
	if gobin != "" {
		if _, _, err = ssh.Run(host, gobin+" version"); err == nil {
			return gobin, nil
		}
	}

	// Go is not available; install it.
	goVersion, err := localGoVersion()
	if err != nil {
		return "", fmt.Errorf("determine Go version: %w", err)
	}

	log.Printf("Go not found on %s; installing Go %s...", host, goVersion)
	installScript := fmt.Sprintf(`set -euo pipefail
VERSION=%s
URL="https://go.dev/dl/go${VERSION}.linux-amd64.tar.gz"
DEST=%s
PARENT="$(dirname "$DEST")"
mkdir -p "$PARENT"
rm -rf "$DEST"
curl -fsSL "$URL" | tar -xz -C "$PARENT"
# The tarball extracts as go/ into $PARENT, which is already $DEST
# when DEST ends with /go. Only rename if names differ.
EXTRACTED="$PARENT/go"
if [ "$EXTRACTED" != "$DEST" ]; then
  mv "$EXTRACTED" "$DEST"
fi
echo "installed Go $("$DEST/bin/go" version)"
`, goVersion, remoteGoDir)

	_, stderr, err := ssh.RunWithStdin(host, "bash -s", installScript)
	if err != nil {
		return "", fmt.Errorf("install Go %s: %s", goVersion, strings.TrimSpace(stderr))
	}

	return remoteGoDir + "/bin/go", nil
}

// localGoVersion returns the Go toolchain version string (e.g. "1.23.4")
// from the local go binary.
func localGoVersion() (string, error) {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return "", err
	}
	// "go1.23.4" -> "1.23.4"
	v := strings.TrimSpace(string(out))
	return strings.TrimPrefix(v, "go"), nil
}
