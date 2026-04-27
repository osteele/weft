package cloudneeds

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

var objectExistsFunc = func(ctx context.Context, client *r2.Client, key string) (bool, error) {
	return client.ObjectExists(ctx, key)
}

const objectExistsTimeout = 20 * time.Second

// ResolveSpecs resolves cloud dependency specs (path:producerJobID) into exact
// R2 keys that can be staged by the cloud agent.
func ResolveSpecs(ctx context.Context, database *sql.DB, client *r2.Client, specs []string) ([]cloud.CloudNeed, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if client == nil {
		return nil, fmt.Errorf("R2 is not configured")
	}

	resolved := make([]cloud.CloudNeed, 0, len(specs))
	for _, spec := range specs {
		parsed, err := runner.ParseNeedsSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("parse cloud need %q: %w", spec, err)
		}
		producer, err := db.GetJobByID(database, parsed.Version)
		if err != nil {
			return nil, fmt.Errorf("lookup producer job %s for %q: %w", ids.FormatJobID(parsed.Version), spec, err)
		}
		if producer == nil {
			return nil, fmt.Errorf("producer job %s for %q not found", ids.FormatJobID(parsed.Version), spec)
		}

		key, err := resolveNeedR2Key(ctx, client, producer.ID, producer.LatestRunID, parsed.Path)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", spec, err)
		}
		resolved = append(resolved, cloud.CloudNeed{
			Spec:  spec,
			Path:  parsed.Path,
			R2Key: key,
		})
	}
	return resolved, nil
}

func resolveNeedR2Key(ctx context.Context, client *r2.Client, jobID int64, latestRunID *int64, relPath string) (string, error) {
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
			exists, err := objectExistsFunc(checkCtx, client, key)
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
