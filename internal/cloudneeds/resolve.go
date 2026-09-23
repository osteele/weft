package cloudneeds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

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
	ErrArtifactPublicationMissing = errors.New("published artifact is missing")
)

var getPublicationReport = func(ctx context.Context, client *r2.Client, key string) ([]byte, error) {
	return client.GetObject(ctx, key)
}

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

		state, stateErr := latestPublicationState(ctx, database, client, producer.ID, producer.LatestRunID)
		if stateErr != nil {
			return nil, fmt.Errorf("%w: resolve publication state for %q: %w", ErrArtifactPublicationUnknown, spec, stateErr)
		}
		if state != nil && state.Sequence > 0 {
			payloadKey, err := readyArtifactPublication(spec, parsed.Path, producer.ID, state)
			if err != nil {
				return nil, err
			}
			needs, err := r2resolve.ResolvePublishedNeed(ctx, client, producer.ID, state.AttemptID, spec, parsed.Path, payloadKey)
			if errors.Is(err, r2resolve.ErrArtifactMissing) {
				return nil, classifyMissingArtifact(spec, parsed.Path, producer.ID, state)
			}
			if err != nil {
				return nil, fmt.Errorf("%w: resolve %q: %w", ErrArtifactPublicationUnknown, spec, err)
			}
			resolved = append(resolved, needs...)
			continue
		}

		// Legacy publications without readiness evidence remain resolvable as
		// single objects. Never infer a complete directory from partial bytes.
		key, err := r2resolve.NeedR2Key(ctx, client, producer.ID, producer.LatestRunID, parsed.Path)
		if err != nil {
			if errors.Is(err, r2resolve.ErrArtifactMissing) {
				return nil, classifyMissingArtifact(spec, parsed.Path, producer.ID, state)
			}
			return nil, fmt.Errorf("%w: resolve %q: %w", ErrArtifactPublicationUnknown, spec, err)
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
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	data, err := getPublicationReport(probeCtx, client, r2keys.JobAttemptPublicationReport(jobID, *runID))
	if err == nil && len(data) > 0 {
		return db.DecodeAttemptPublicationReport(data, jobID, *runID)
	}
	state, stateErr := db.GetAttemptPublicationState(database, *runID)
	if stateErr != nil {
		return nil, stateErr
	}
	if err != nil && !r2.IsNotFound(err) && (state == nil || state.Sequence == 0) {
		return nil, err
	}
	if state != nil {
		if state.JobID != jobID || state.AttemptID != *runID {
			return nil, db.ErrPublicationAttemptMismatch
		}
		return state, nil
	}
	return nil, nil
}

func readyArtifactPublication(spec, artifactPath string, producerID int64, state *db.AttemptPublicationState) (string, error) {
	wanted := cleanArtifactPath(artifactPath)
	var matched *db.AttemptPublicationArtifact
	for i := range state.Artifacts {
		artifact := &state.Artifacts[i]
		if cleanArtifactPath(artifact.Path) != wanted {
			continue
		}
		if matched != nil {
			return "", fmt.Errorf("%w: ambiguous publication for %q", ErrArtifactPublicationUnknown, spec)
		}
		matched = artifact
	}
	if matched != nil {
		if matched.State == db.PublicationStateReady {
			return matched.PayloadKey, nil
		}
		return "", classifyMissingArtifact(spec, artifactPath, producerID, state)
	}
	if state.DrainState == db.PublicationStateReady {
		return "", nil
	}
	return "", classifyMissingArtifact(spec, artifactPath, producerID, state)
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
			return fmt.Errorf("%w: %s reported %q ready, but no file or directory was found",
				ErrArtifactPublicationMissing, producer, artifactPath)
		default:
			return fmt.Errorf("%w: requires %q from %s", ErrArtifactPublicationUnknown, spec, producer)
		}
	}
	switch state.DrainState {
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
	cleaned := path.Clean(strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(value, `\\`, "/")), "/"))
	return strings.TrimPrefix(cleaned, "./")
}
