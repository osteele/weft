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
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

const objectExistsTimeout = 20 * time.Second

// Lister lists R2 objects under a prefix. *r2.Client satisfies it;
// command-layer fakes can too.
type Lister interface {
	ListObjects(ctx context.Context, prefix string) ([]r2.ObjectInfo, error)
}

// Store adds the existence probe NeedR2Key needs on top of listing.
type Store interface {
	Lister
	ObjectExists(ctx context.Context, key string) (bool, error)
}

// ErrArtifactMissing reports that no R2 key under the producer's prefixes
// matches the requested artifact path. Callers that have producer/spec
// context (e.g. cloudneeds.ResolveSpecs) can use errors.Is to discriminate
// this from transient lookup errors and surface a user-facing message.
var ErrArtifactMissing = errors.New("artifact not found in cloud outputs")

// ObjectExistsFunc is the R2 existence-check used by NeedR2Key. Exported so
// tests can stub the R2 round-trip without a live client.
var ObjectExistsFunc = func(ctx context.Context, client Store, key string) (bool, error) {
	return client.ObjectExists(ctx, key)
}

// ListRunIDsFunc enumerates run IDs that have R2 keys under
// jobs/<jobID>/runs/. Used by the fallback path when latest_run_id and
// run-zero both miss. Exported so tests can stub without a live S3 client.
var ListRunIDsFunc = func(ctx context.Context, client Lister, jobID int64) ([]int64, error) {
	prefix := r2keys.JobRunsPrefix(jobID)
	objects, err := client.ListObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	seen := map[int64]struct{}{}
	var runIDs []int64
	for _, obj := range objects {
		rest := strings.TrimPrefix(obj.Key, prefix)
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			continue
		}
		var runID int64
		if _, parseErr := fmt.Sscanf(rest[:slash], "%d", &runID); parseErr != nil || runID <= 0 {
			continue
		}
		if _, ok := seen[runID]; ok {
			continue
		}
		seen[runID] = struct{}{}
		runIDs = append(runIDs, runID)
	}
	return runIDs, nil
}

// NeedR2Key probes R2 for a producer's artifact and returns the concrete
// key. It tries the latest-run prefix first (if known), then run-zero, and
// within each run the artifact-files prefix before the outputs prefix. If
// neither candidate exists, it falls back to listing the producer's
// runs/ subdirectories on R2 and probing each — this catches the
// run_id-mismatch case where an attempt was canceled/superseded after
// uploading artifacts (latest_run_id moves on, but the .complete and
// artifact files stay under the run that actually ran).
//
// Returns an error if no candidate exists.
func NeedR2Key(ctx context.Context, client Store, jobID int64, latestRunID *int64, relPath string) (string, error) {
	tried := map[int64]struct{}{}
	runIDs := []int64{0}
	if latestRunID != nil && *latestRunID > 0 {
		runIDs = append([]int64{*latestRunID}, runIDs...)
	}
	artifactRel := artifacts.LocalRelativePath(relPath)
	outputRel := path.Clean(strings.TrimPrefix(relPath, "/"))

	tryRun := func(runID int64) (string, bool, error) {
		if _, seen := tried[runID]; seen {
			return "", false, nil
		}
		tried[runID] = struct{}{}
		keys := []string{
			r2keys.JobAttemptArtifactFilesPrefix(jobID, runID) + artifactRel,
			r2keys.JobAttemptOutputsPrefix(jobID, runID) + outputRel,
		}
		for _, key := range keys {
			checkCtx, cancel := context.WithTimeout(ctx, objectExistsTimeout)
			exists, err := ObjectExistsFunc(checkCtx, client, key)
			cancel()
			if err != nil {
				return "", false, fmt.Errorf("check %s: %w", key, err)
			}
			if exists {
				return key, true, nil
			}
		}
		return "", false, nil
	}

	for _, runID := range runIDs {
		if key, ok, err := tryRun(runID); err != nil {
			return "", err
		} else if ok {
			return key, nil
		}
	}

	// Fallback: scan actual runs/ subdirectories. The producer's
	// supersede/reuse history can leave latest_run_id pointing at an
	// attempt that never uploaded anything; the run that did is still
	// discoverable on R2.
	listCtx, cancel := context.WithTimeout(ctx, objectExistsTimeout)
	defer cancel()
	runIDs, err := ListRunIDsFunc(listCtx, client, jobID)
	if err != nil {
		return "", fmt.Errorf("list runs for job %d: %w", jobID, err)
	}
	for _, runID := range runIDs {
		if key, ok, err := tryRun(runID); err != nil {
			return "", err
		} else if ok {
			return key, nil
		}
	}

	return "", fmt.Errorf("%w: %q", ErrArtifactMissing, relPath)
}
