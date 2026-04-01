package agentdeploy

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
)

// BuildViaFly builds the agent binary for the given platform using the Fly.io
// builder. Requires WEFT_FLY_BUILDER_APP and WEFT_FLY_BUILDER_MACHINE env vars
// (validated by the script). Returns the local cache path to the built binary.
func BuildViaFly(version, goos, goarch string, output io.Writer) (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", fmt.Errorf("locate repo root: %w", err)
	}

	script := filepath.Join(root, "scripts", "build-agent-on-fly.sh")
	outputPath := CachePath(version, goos, goarch, "")

	cmd := exec.Command(script, version, outputPath)
	cmd.Dir = root
	cmd.Env = mergeEnvVars(loadRepoEnvVars(root))
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("fly build failed: %w", err)
	}

	return outputPath, nil
}
