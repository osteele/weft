package agentdeploy

import (
	"context"
	"fmt"
	"os"

	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// EnsureAgentInR2 uploads the agent binary for the given version to R2.
// Always re-uploads to ensure the R2 copy matches the locally embedded binary,
// since the version hash is based on commit history and may not change when
// the binary is rebuilt (e.g., after `just build-agents` without a new commit).
func EnsureAgentInR2(ctx context.Context, r2Client *r2.Client, version, goos, goarch string) (string, error) {
	key := r2keys.AgentBinary(version, goos, goarch)

	localPath, err := EnsureBuilt(version, goos, goarch)
	if err != nil {
		return "", fmt.Errorf("build agent: %w", err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open agent binary: %w", err)
	}
	defer f.Close()

	if err := r2Client.PutObject(ctx, key, f, "application/octet-stream"); err != nil {
		return "", fmt.Errorf("upload agent binary: %w", err)
	}

	return key, nil
}
