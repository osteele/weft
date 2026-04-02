package agentdeploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

// EnsureAgentProgressFunc receives coarse agent upload phase updates.
// Phases are descriptive labels such as "checking cache", "building agent",
// "uploading agent", and "ready".
type EnsureAgentProgressFunc func(phase string)

// EnsureAgentInR2 uploads the agent binary for the given version to R2.
// If the object is already present, it skips build and upload.
// If the binary is not in the local cache, it attempts to build via the Fly builder.
func EnsureAgentInR2(ctx context.Context, r2Client *r2.Client, version, goos, goarch string, output io.Writer) (string, error) {
	return EnsureAgentInR2WithProgress(ctx, r2Client, version, goos, goarch, output, nil)
}

// EnsureAgentInR2WithProgress uploads the agent binary for the given version to
// R2 and reports coarse phase changes via onProgress.
func EnsureAgentInR2WithProgress(ctx context.Context, r2Client *r2.Client, version, goos, goarch string, output io.Writer, onProgress EnsureAgentProgressFunc) (string, error) {
	if onProgress == nil {
		onProgress = func(string) {}
	}
	key := dataplane.AgentBinary(version, goos, goarch)

	onProgress("checking cache")
	exists, err := r2Client.ObjectExists(ctx, key)
	if err != nil {
		return "", fmt.Errorf("check agent binary exists: %w", err)
	}
	if exists {
		onProgress("ready")
		return key, nil
	}

	localPath, err := EnsureBuilt(version, goos, goarch)
	if errors.Is(err, ErrAgentNotAvailable) {
		onProgress("building agent")
		slog.Info("agent binary not in cache, building via Fly builder", "component", "agentdeploy")
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

	onProgress("uploading agent")
	if err := r2Client.PutObject(ctx, key, f, "application/octet-stream"); err != nil {
		return "", fmt.Errorf("upload agent binary: %w", err)
	}

	onProgress("ready")
	return key, nil
}
