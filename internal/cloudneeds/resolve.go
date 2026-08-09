package cloudneeds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
	"github.com/osteele/weft/internal/runner"
)

var (
	ErrArtifactPublicationPending = errors.New("artifact publication is pending")
	ErrArtifactPublicationUnknown = errors.New("artifact publication is unknown")
	ErrArtifactPublicationFailed  = errors.New("artifact publication failed")
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
		if parsed.IsAsset() {
			asset, err := db.GetNamedAssetByName(database, parsed.AssetName)
			if err != nil {
				return nil, fmt.Errorf("resolve %q: %w", spec, err)
			}
			resolved = append(resolved, cloud.CloudNeed{
				Spec:        spec,
				Path:        asset.TargetPath,
				R2Key:       r2keys.NamedAsset(asset.ContentHash),
				ContentType: asset.ContentType,
			})
			continue
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
			if errors.Is(err, r2resolve.ErrArtifactMissing) {
				state, stateErr := latestPublicationState(ctx, database, client, producer.ID, producer.LatestRunID)
				if stateErr != nil {
					return nil, fmt.Errorf("resolve publication state for %q: %w", spec, stateErr)
				}
				return nil, classifyMissingArtifact(spec, parsed.Path, producer.ID, state)
			}
			return nil, fmt.Errorf("resolve %q: %w", spec, err)
		}
		resolved = append(resolved, cloud.CloudNeed{
			Spec:  spec,
			Path:  parsed.Path,
			R2Key: key,
		})
	}
	return resolved, nil
}

func latestPublicationState(ctx context.Context, database *sql.DB, client *r2.Client, jobID int64, runID *int64) (*db.AttemptPublicationState, error) {
	if runID == nil || *runID <= 0 {
		return nil, nil
	}
	data, err := client.GetObject(ctx, r2keys.JobAttemptPublicationReport(jobID, *runID))
	if err == nil && len(data) > 0 {
		return db.DecodeAttemptPublicationReport(data, jobID, *runID)
	}
	return db.GetAttemptPublicationState(database, *runID)
}

func classifyMissingArtifact(spec, artifactPath string, producerID int64, state *db.AttemptPublicationState) error {
	producer := ids.FormatJobID(producerID)
	if state == nil {
		return fmt.Errorf("%w: requires %q; readiness for %s has not been observed",
			ErrArtifactPublicationUnknown, spec, producer)
	}
	wanted := cleanArtifactPath(artifactPath)
	for _, artifact := range state.Artifacts {
		if cleanArtifactPath(artifact.Path) != wanted {
			continue
		}
		switch artifact.State {
		case db.PublicationStatePending:
			return fmt.Errorf("%w: requires %q from %s", ErrArtifactPublicationPending, spec, producer)
		case db.PublicationStateFailed:
			return fmt.Errorf("%w: requires %q that %s did not publish", ErrArtifactPublicationFailed, spec, producer)
		case db.PublicationStateReady:
			return fmt.Errorf("%w: %s reported %q ready, but its object was not observable",
				ErrArtifactPublicationUnknown, producer, artifactPath)
		default:
			return fmt.Errorf("%w: requires %q from %s", ErrArtifactPublicationUnknown, spec, producer)
		}
	}
	switch state.RequiredArtifactsState {
	case db.PublicationStatePending:
		return fmt.Errorf("%w: requires %q from %s", ErrArtifactPublicationPending, spec, producer)
	case db.PublicationStateFailed:
		return fmt.Errorf("%w: requires %q that %s did not publish", ErrArtifactPublicationFailed, spec, producer)
	case db.PublicationStateReady:
		return fmt.Errorf("%w: requires %q that %s did not produce", ErrArtifactPublicationFailed, spec, producer)
	default:
		detail := strings.TrimSpace(state.UnknownReason)
		if detail == "" {
			detail = "publication readiness was not observed"
		}
		return fmt.Errorf("%w: requires %q from %s: %s", ErrArtifactPublicationUnknown, spec, producer, detail)
	}
}

func cleanArtifactPath(value string) string {
	cleaned := path.Clean(strings.TrimSpace(strings.ReplaceAll(value, `\\`, "/")))
	return strings.TrimPrefix(cleaned, "./")
}
