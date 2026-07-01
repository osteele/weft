package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
)

const resumeCloudNeedsListTimeout = 20 * time.Second

var resolveResumeCloudNeedsFunc = resolveResumeCloudNeeds

// appendResumeCloudNeeds adds R2-staged files from this job's predecessor
// attempt. It restores a fresh rental instance's workdir to the last
// checkpointed output tree before the user command starts.
func appendResumeCloudNeeds(
	ctx context.Context,
	database *sql.DB,
	client *r2.Client,
	job *db.Job,
	cloudNeeds []cloud.CloudNeed,
) ([]cloud.CloudNeed, bool, error) {
	resumeNeeds, err := resolveResumeCloudNeedsFunc(ctx, database, client, job)
	if err != nil {
		return nil, false, err
	}
	if len(resumeNeeds) == 0 {
		return cloudNeeds, false, nil
	}
	seenPaths := make(map[string]bool, len(cloudNeeds)+len(resumeNeeds))
	for _, need := range cloudNeeds {
		seenPaths[need.Path] = true
	}
	added := false
	for _, need := range resumeNeeds {
		if seenPaths[need.Path] {
			continue
		}
		cloudNeeds = append(cloudNeeds, need)
		seenPaths[need.Path] = true
		added = true
	}
	return cloudNeeds, added, nil
}

func resolveResumeCloudNeeds(ctx context.Context, database *sql.DB, client *r2.Client, job *db.Job) ([]cloud.CloudNeed, error) {
	if job == nil {
		return nil, fmt.Errorf("job is nil")
	}
	if job.LatestRunID == nil || *job.LatestRunID <= 0 {
		return nil, nil
	}

	for attemptID, depth := *job.LatestRunID, 0; attemptID > 0 && depth < 16; depth++ {
		predecessorID, ok, err := predecessorAttemptID(database, job.ID, attemptID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		if client == nil {
			return nil, fmt.Errorf("R2 client is not configured for resume staging")
		}
		needs, err := listAttemptResumeCloudNeeds(ctx, client, job.ID, predecessorID)
		if err != nil {
			return nil, fmt.Errorf("list resume artifacts for %s predecessor attempt %d: %w", ids.FormatJobID(job.ID), predecessorID, err)
		}
		if len(needs) > 0 {
			return needs, nil
		}
		attemptID = predecessorID
	}
	return nil, nil
}

func predecessorAttemptID(database *sql.DB, jobID, attemptID int64) (int64, bool, error) {
	var predecessor sql.NullInt64
	err := database.QueryRow(
		`SELECT predecessor_attempt_id
		   FROM job_attempts
		  WHERE id = ? AND job_id = ?`,
		attemptID, jobID,
	).Scan(&predecessor)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !predecessor.Valid || predecessor.Int64 <= 0 {
		return 0, false, nil
	}
	return predecessor.Int64, true, nil
}

func listAttemptResumeCloudNeeds(ctx context.Context, client r2resolve.Lister, jobID, runID int64) ([]cloud.CloudNeed, error) {
	outputsPrefix := r2keys.JobAttemptOutputsPrefix(jobID, runID)
	artifactsPrefix := r2keys.JobAttemptArtifactFilesPrefix(jobID, runID)

	needsByPath := map[string]cloud.CloudNeed{}
	if err := appendResumeNeedsForPrefix(ctx, client, jobID, outputsPrefix, "", needsByPath); err != nil {
		return nil, err
	}
	if err := appendResumeNeedsForPrefix(ctx, client, jobID, artifactsPrefix, "", needsByPath); err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(needsByPath))
	for p := range needsByPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	needs := make([]cloud.CloudNeed, 0, len(paths))
	for _, p := range paths {
		needs = append(needs, needsByPath[p])
	}
	return needs, nil
}

func appendResumeNeedsForPrefix(
	ctx context.Context,
	client r2resolve.Lister,
	jobID int64,
	prefix string,
	pathPrefix string,
	needsByPath map[string]cloud.CloudNeed,
) error {
	listCtx, cancel := context.WithTimeout(ctx, resumeCloudNeedsListTimeout)
	defer cancel()
	objects, err := client.ListObjects(listCtx, prefix)
	if err != nil {
		return fmt.Errorf("list %s: %w", prefix, err)
	}
	for _, obj := range objects {
		rel, ok := cleanResumeRelPath(path.Join(pathPrefix, strings.TrimPrefix(obj.Key, prefix)))
		if !ok {
			continue
		}
		if _, seen := needsByPath[rel]; seen {
			continue
		}
		needsByPath[rel] = cloud.CloudNeed{
			Spec:  fmt.Sprintf("%s:%d", rel, jobID),
			Path:  rel,
			R2Key: obj.Key,
		}
	}
	return nil
}

func cleanResumeRelPath(raw string) (string, bool) {
	cleaned := path.Clean(strings.TrimPrefix(strings.TrimSpace(raw), "/"))
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", false
	}
	return cleaned, true
}
