package agentdeploy

import (
	"context"
	"fmt"
	"os"

	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// EnsureAgentInR2 uploads the agent binary for the given version to R2.
// If the object is already present, it skips build and upload.
func EnsureAgentInR2(ctx context.Context, r2Client *r2.Client, version, goos, goarch string) (string, error) {
	key := r2keys.AgentBinary(version, goos, goarch)

	exists, err := r2Client.ObjectExists(ctx, key)
	if err != nil {
		return "", fmt.Errorf("check agent binary exists: %w", err)
	}
	if exists {
		return key, nil
	}

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
