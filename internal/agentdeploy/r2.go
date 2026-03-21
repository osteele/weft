package agentdeploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// EnsureAgentInR2 uploads the agent binary for the given version to R2.
// If the object is already present, it skips build and upload.
// If the binary is not in the local cache, it attempts to build via the Fly builder.
func EnsureAgentInR2(ctx context.Context, r2Client *r2.Client, version, goos, goarch string, output io.Writer) (string, error) {
	key := r2keys.AgentBinary(version, goos, goarch)

	exists, err := r2Client.ObjectExists(ctx, key)
	if err != nil {
		return "", fmt.Errorf("check agent binary exists: %w", err)
	}
	if exists {
		return key, nil
	}

	localPath, err := EnsureBuilt(version, goos, goarch, "")
	if errors.Is(err, ErrAgentNotAvailable) {
		log.Printf("agent binary not in cache; building via Fly builder...")
		localPath, err = BuildViaFly(version, goos, goarch, output)
		if err != nil {
			return "", fmt.Errorf("build agent: %w", err)
		}
	} else if err != nil {
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
