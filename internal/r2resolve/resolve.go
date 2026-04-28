// Package r2resolve owns the policy for finding a producer's artifact in R2:
// which prefix to look under (latest run vs. legacy run-zero, artifact-files
// prefix vs. outputs prefix) and in what order.
//
// It lives apart from internal/cloudneeds so callers in internal/ops can use
// it without dragging in cloudneeds → runner → placement → ops, which would
// be an import cycle.
package r2resolve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

const objectExistsTimeout = 20 * time.Second

// ObjectExistsFunc is the R2 existence-check used by NeedR2Key. Exported so
// tests can stub the R2 round-trip without a live client.
var ObjectExistsFunc = func(ctx context.Context, client *r2.Client, key string) (bool, error) {
	return client.ObjectExists(ctx, key)
}

// NeedR2Key probes R2 for a producer's artifact and returns the concrete
// key. It tries the latest-run prefix first (if known), then run-zero, and
// within each run the artifact-files prefix before the outputs prefix.
// Returns an error if no candidate exists.
func NeedR2Key(ctx context.Context, client *r2.Client, jobID int64, latestRunID *int64, relPath string) (string, error) {
	runIDs := []int64{0}
	if latestRunID != nil && *latestRunID > 0 {
		runIDs = append([]int64{*latestRunID}, runIDs...)
	}
	artifactRel := artifacts.LocalRelativePath(relPath)
	outputRel := strings.TrimPrefix(relPath, "/")

	for _, runID := range runIDs {
		keys := []string{
			r2keys.JobAttemptArtifactFilesPrefix(jobID, runID) + artifactRel,
			r2keys.JobAttemptOutputsPrefix(jobID, runID) + outputRel,
		}
		for _, key := range keys {
			checkCtx, cancel := context.WithTimeout(ctx, objectExistsTimeout)
			exists, err := ObjectExistsFunc(checkCtx, client, key)
			cancel()
			if err != nil {
				return "", fmt.Errorf("check %s: %w", key, err)
			}
			if exists {
				return key, nil
			}
		}
	}
	return "", fmt.Errorf("artifact %q not found in cloud outputs", relPath)
}
