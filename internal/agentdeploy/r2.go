package agentdeploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

// ErrNoCachedAgent is returned by latestAgentInR2 when no cached agent binary
// exists for the requested platform.
var ErrNoCachedAgent = errors.New("no cached agent binary found in R2")

// latestAgentInR2 lists agents/<version>/<goos>-<goarch> objects in R2 and
// returns the key with the most recent LastModified time, or ErrNoCachedAgent
// if none match. Used as a fallback when an exact-version build is unavailable
// (e.g. the local machine can't cross-compile and remote builders are
// unreachable).
func latestAgentInR2(ctx context.Context, r2Client *r2.Client, goos, goarch string) (string, r2.ObjectInfo, error) {
	objects, err := r2Client.ListObjects(ctx, "agents/")
	if err != nil {
		return "", r2.ObjectInfo{}, fmt.Errorf("list cached agents: %w", err)
	}
	suffix := "/" + goos + "-" + goarch
	var best r2.ObjectInfo
	found := false
	for _, obj := range objects {
		if !strings.HasSuffix(obj.Key, suffix) {
			continue
		}
		if !found || obj.LastModified.After(best.LastModified) {
			best = obj
			found = true
		}
	}
	if !found {
		return "", r2.ObjectInfo{}, ErrNoCachedAgent
	}
	return best.Key, best, nil
}

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
	if err != nil {
		if fallbackKey, fbInfo, fbErr := latestAgentInR2(ctx, r2Client, goos, goarch); fbErr == nil {
			fmt.Fprintf(output, "warning: build failed (%v); using stale cached agent %s (uploaded %s)\n",
				err, fallbackKey, fbInfo.LastModified.Format(time.RFC3339))
			onProgress("ready")
			return fallbackKey, nil
		}
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
