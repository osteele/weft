package campaign

import (
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

func persistCloudSourceDispatch(database *sql.DB, job *db.Job, agentVersion string) error {
	if job == nil || job.Metadata == nil || job.Metadata.Source == nil || job.Metadata.Source.Pin == nil {
		return nil
	}
	pin := job.Metadata.Source.Pin
	job.Metadata.Source.Execution = &db.JobSourceExecutionMetadata{
		SubmittedIdentityKind: db.SourceIdentityManifestV2,
		SubmittedSHA256:       pin.Hash,
		DispatchMode:          "pinned_cloud_manifest",
		IdentityKind:          db.SourceIdentityManifestV2,
		DispatchedSHA256:      pin.Hash,
		Verification:          db.SourceVerificationPending,
		AgentVersion:          agentVersion,
		RootCount:             len(pin.Roots),
	}
	return db.SetJobMetadata(database, job.ID, job.Metadata)
}

func persistCloudSourceVerification(database *sql.DB, jobID, attemptID int64, failureReason string, observedAt time.Time) {
	attempts, err := db.ListAttempts(database, jobID)
	if err != nil {
		slog.Warn("load attempts for source verification", "component", "source-provenance", "job_id", jobID, "attempt_id", attemptID, "error", err)
		return
	}
	for _, attempt := range attempts {
		if attempt.ID != attemptID || attempt.Metadata == nil || attempt.Metadata.Source == nil || attempt.Metadata.Source.Execution == nil {
			continue
		}
		execution := attempt.Metadata.Source.Execution
		if execution.DispatchMode != "pinned_cloud_manifest" {
			return
		}
		if strings.HasPrefix(failureReason, "source_restore_failed:") {
			execution.Verification = db.SourceVerificationObjectsUnavailable
			if strings.Contains(failureReason, "mismatch") || strings.Contains(failureReason, "does not match") {
				execution.Verification = db.SourceVerificationMismatch
			}
		} else {
			execution.VerifiedSHA256 = execution.DispatchedSHA256
			execution.Verification = db.SourceVerificationVerified
		}
		if !observedAt.IsZero() {
			execution.VerifiedAt = observedAt.Unix()
		}
		if err := db.SetJobAttemptMetadata(database, jobID, attemptID, attempt.Metadata); err != nil {
			slog.Warn("persist cloud source verification", "component", "source-provenance", "job_id", jobID, "attempt_id", attemptID, "error", err)
		}
		return
	}
	slog.Warn("source verification attempt metadata unavailable", "component", "source-provenance", "job_id", jobID, "attempt_id", attemptID)
}
