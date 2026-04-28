package cloudneeds

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2resolve"
	"github.com/osteele/weft/internal/runner"
)

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

		key, err := r2resolve.NeedR2Key(ctx, client, producer.ID, producer.LatestRunID, parsed.Path)
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
