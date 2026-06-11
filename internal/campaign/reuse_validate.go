package campaign

import (
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

// ValidateJobSourceForCloud checks — without claiming or uploading anything —
// that the job's source tree (plus declared local: inputs) can be shipped to
// a cloud instance. It must run BEFORE a job is claimed onto a launch: a
// deterministic rejection (no local source, source over the size cap) that
// surfaces only after the claim costs an attempt row per retry and, with no
// backoff, produced the wj2812 churn of hundreds of fabricated attempts.
// Over-limit sources return an error wrapping sync.ErrSourceTooLarge.
func ValidateJobSourceForCloud(job *db.Job) error {
	sourceDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	if sourceDir == "" || workdir.IsContainerPath(sourceDir) {
		return fmt.Errorf("job %s has no local source directory (working_dir=%q)",
			ids.FormatJobID(job.ID), job.EffectiveWorkingDir())
	}
	total, overlays, err := weftsync.EstimateSnapshotBytesWithInputs(sourceDir, job.Inputs)
	if err != nil {
		return fmt.Errorf("estimate source size for job %s: %w", ids.FormatJobID(job.ID), err)
	}
	if total > weftsync.MaxSourceTarballBytes {
		return fmt.Errorf("job %s: %w", ids.FormatJobID(job.ID), weftsync.SourceSizeLimitError(total, overlays))
	}
	return nil
}
