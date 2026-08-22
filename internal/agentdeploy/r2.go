package agentdeploy

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

type agentObjectStore interface {
	ObjectExists(context.Context, string) (bool, error)
	PutObject(context.Context, string, io.Reader, string) error
}

type agentBuilder func(string, string, string, io.Writer, EnsureAgentProgressFunc) (string, error)

// EnsureAgentProgressFunc receives coarse agent upload phase updates.
// Phases are descriptive labels such as "checking cache", "building agent",
// "uploading agent", and "ready".
type EnsureAgentProgressFunc func(phase string)

// EnsureAgentInR2 uploads the agent binary for the given version to R2.
// If the object is already present, it skips build and upload.
func EnsureAgentInR2(ctx context.Context, r2Client *r2.Client, version, goos, goarch string, output io.Writer) (string, error) {
	return EnsureAgentInR2WithProgress(ctx, r2Client, version, goos, goarch, output, nil)
}

// EnsureAgentInR2WithProgress uploads the agent binary for the given version to
// R2 and reports coarse phase changes via onProgress.
func EnsureAgentInR2WithProgress(ctx context.Context, r2Client *r2.Client, version, goos, goarch string, output io.Writer, onProgress EnsureAgentProgressFunc) (string, error) {
	return ensureAgentInR2WithProgress(ctx, r2Client, version, goos, goarch, output, onProgress, EnsureBuiltWithProgress)
}

func ensureAgentInR2WithProgress(ctx context.Context, store agentObjectStore, version, goos, goarch string, output io.Writer, onProgress EnsureAgentProgressFunc, build agentBuilder) (string, error) {
	if onProgress == nil {
		onProgress = func(string) {}
	}
	key := dataplane.AgentBinary(version, goos, goarch)

	onProgress("checking cache")
	exists, err := store.ObjectExists(ctx, key)
	if err != nil {
		return "", fmt.Errorf("check agent binary exists: %w", err)
	}
	if exists {
		onProgress("ready")
		return key, nil
	}

	localPath, err := build(version, goos, goarch, output, onProgress)
	if err != nil {
		return "", fmt.Errorf("build exact agent %s for %s/%s: %w", version, goos, goarch, err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open agent binary: %w", err)
	}
	defer f.Close()

	onProgress("uploading agent")
	if err := store.PutObject(ctx, key, f, "application/octet-stream"); err != nil {
		return "", fmt.Errorf("upload agent binary: %w", err)
	}

	onProgress("ready")
	return key, nil
}
