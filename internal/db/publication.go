package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	PublicationStatePending = "pending"
	PublicationStateReady   = "ready"
	PublicationStateFailed  = "failed"
	PublicationStateUnknown = "unknown"

	PublicationExecutionPending  = "pending"
	PublicationExecutionComplete = "complete"
	PublicationExecutionUnknown  = "unknown"
)

var ErrPublicationAttemptMismatch = errors.New("publication report does not match attempt")

type AttemptPublicationArtifact struct {
	Name       string `json:"name"`
	Path       string `json:"path,omitempty"`
	State      string `json:"state"`
	ReadyAt    *int64 `json:"ready_at_unix,omitempty"`
	PayloadKey string `json:"payload_key,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Sequence   int64  `json:"sequence"`
}

type AttemptPublicationState struct {
	AttemptID                int64                        `json:"attempt_id"`
	JobID                    int64                        `json:"job_id"`
	Sequence                 int64                        `json:"sequence"`
	ObservedAt               int64                        `json:"observed_at_unix"`
	ExecutionState           string                       `json:"execution_state"`
	ExecutionCompletedAt     *int64                       `json:"execution_completed_at_unix,omitempty"`
	RequiredArtifactsState   string                       `json:"required_artifacts_state"`
	RequiredArtifactsReadyAt *int64                       `json:"required_artifacts_ready_at_unix,omitempty"`
	DrainState               string                       `json:"drain_state"`
	DrainCompletedAt         *int64                       `json:"drain_completed_at_unix,omitempty"`
	QueuedItems              *int                         `json:"queued_items,omitempty"`
	QueuedBytes              *int64                       `json:"queued_bytes,omitempty"`
	InflightItems            *int                         `json:"inflight_items,omitempty"`
	InflightBytes            *int64                       `json:"inflight_bytes,omitempty"`
	OldestQueuedAgeMS        *int64                       `json:"oldest_queued_age_ms,omitempty"`
	RetainedBytes            *int64                       `json:"retained_bytes,omitempty"`
	WorkerLimit              *int                         `json:"worker_limit,omitempty"`
	WorkersBusy              *int                         `json:"workers_busy,omitempty"`
	LastProgressAt           *int64                       `json:"last_progress_at_unix,omitempty"`
	UnknownReason            string                       `json:"unknown_reason,omitempty"`
	Detail                   string                       `json:"detail,omitempty"`
	Artifacts                []AttemptPublicationArtifact `json:"artifacts,omitempty"`
}

func validReadinessState(state string) bool {
	switch state {
	case PublicationStatePending, PublicationStateReady, PublicationStateFailed, PublicationStateUnknown:
		return true
	default:
		return false
	}
}

func validExecutionState(state string) bool {
	switch state {
	case PublicationExecutionPending, PublicationExecutionComplete, PublicationExecutionUnknown:
		return true
	default:
		return false
	}
}

func terminalPublicationState(state string) bool {
	return state == PublicationStateReady || state == PublicationStateFailed
}

func preserveTerminalState(oldState string, oldAt *int64, newState string, newAt *int64) (string, *int64) {
	if terminalPublicationState(oldState) {
		return oldState, oldAt
	}
	return newState, newAt
}

// UpsertAttemptPublicationState persists a newer agent report. Reports are
// attempt-scoped: attemptID is the run ID carried by R2 keys and completion
// records. Stale sequences are ignored. Once a facet or artifact reaches a
// terminal state, a later report cannot downgrade or replace that outcome.
func UpsertAttemptPublicationState(database *sql.DB, state *AttemptPublicationState) (bool, error) {
	if state == nil {
		return false, errors.New("publication state is nil")
	}
	if state.AttemptID <= 0 || state.JobID <= 0 || state.Sequence <= 0 || state.ObservedAt <= 0 {
		return false, errors.New("publication state requires positive attempt, job, sequence, and observed time")
	}
	if !validExecutionState(state.ExecutionState) {
		return false, fmt.Errorf("invalid execution state %q", state.ExecutionState)
	}
	if !validReadinessState(state.RequiredArtifactsState) || !validReadinessState(state.DrainState) {
		return false, errors.New("invalid publication readiness state")
	}
	for _, artifact := range state.Artifacts {
		if artifact.Name == "" || !validReadinessState(artifact.State) {
			return false, fmt.Errorf("invalid publication artifact %q state %q", artifact.Name, artifact.State)
		}
	}

	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var attemptJobID int64
	if err := tx.QueryRow(`SELECT job_id FROM job_attempts WHERE id = ?`, state.AttemptID).Scan(&attemptJobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrPublicationAttemptMismatch
		}
		return false, err
	}
	if attemptJobID != state.JobID {
		return false, ErrPublicationAttemptMismatch
	}

	var oldSequence int64
	var oldExecution, oldRequired, oldDrain string
	var oldExecutionAt, oldRequiredAt, oldDrainAt sql.NullInt64
	err = tx.QueryRow(`
		SELECT sequence, execution_state, execution_completed_at,
		       required_artifacts_state, required_artifacts_ready_at,
		       drain_state, drain_completed_at
		FROM attempt_publication_state WHERE attempt_id = ?`, state.AttemptID).Scan(
		&oldSequence, &oldExecution, &oldExecutionAt,
		&oldRequired, &oldRequiredAt, &oldDrain, &oldDrainAt,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if state.Sequence <= oldSequence {
			return false, nil
		}
		if oldExecution == PublicationExecutionComplete {
			state.ExecutionState = oldExecution
			state.ExecutionCompletedAt = nullableInt64Ptr(oldExecutionAt)
		}
		state.RequiredArtifactsState, state.RequiredArtifactsReadyAt = preserveTerminalState(
			oldRequired, nullableInt64Ptr(oldRequiredAt), state.RequiredArtifactsState, state.RequiredArtifactsReadyAt)
		state.DrainState, state.DrainCompletedAt = preserveTerminalState(
			oldDrain, nullableInt64Ptr(oldDrainAt), state.DrainState, state.DrainCompletedAt)
	}

	_, err = tx.Exec(`
		INSERT INTO attempt_publication_state (
			attempt_id, sequence, observed_at, execution_state, execution_completed_at,
			required_artifacts_state, required_artifacts_ready_at,
			drain_state, drain_completed_at, queued_items, queued_bytes,
			inflight_items, inflight_bytes, oldest_queued_age_ms, retained_bytes,
			worker_limit, workers_busy, last_progress_at, unknown_reason, detail
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(attempt_id) DO UPDATE SET
			sequence = excluded.sequence, observed_at = excluded.observed_at,
			execution_state = excluded.execution_state,
			execution_completed_at = excluded.execution_completed_at,
			required_artifacts_state = excluded.required_artifacts_state,
			required_artifacts_ready_at = excluded.required_artifacts_ready_at,
			drain_state = excluded.drain_state,
			drain_completed_at = excluded.drain_completed_at,
			queued_items = excluded.queued_items, queued_bytes = excluded.queued_bytes,
			inflight_items = excluded.inflight_items, inflight_bytes = excluded.inflight_bytes,
			oldest_queued_age_ms = excluded.oldest_queued_age_ms,
			retained_bytes = excluded.retained_bytes, worker_limit = excluded.worker_limit,
			workers_busy = excluded.workers_busy, last_progress_at = excluded.last_progress_at,
			unknown_reason = excluded.unknown_reason, detail = excluded.detail`,
		state.AttemptID, state.Sequence, state.ObservedAt,
		state.ExecutionState, state.ExecutionCompletedAt,
		state.RequiredArtifactsState, state.RequiredArtifactsReadyAt,
		state.DrainState, state.DrainCompletedAt,
		state.QueuedItems, state.QueuedBytes, state.InflightItems, state.InflightBytes,
		state.OldestQueuedAgeMS, state.RetainedBytes, state.WorkerLimit, state.WorkersBusy,
		state.LastProgressAt, state.UnknownReason, state.Detail,
	)
	if err != nil {
		return false, err
	}

	for i := range state.Artifacts {
		artifact := &state.Artifacts[i]
		var oldArtifactState string
		var oldArtifactAt sql.NullInt64
		var artifactSequence int64
		err := tx.QueryRow(`
			SELECT state, ready_at, sequence
			FROM attempt_publication_artifacts
			WHERE attempt_id = ? AND name = ?`, state.AttemptID, artifact.Name).Scan(
			&oldArtifactState, &oldArtifactAt, &artifactSequence)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if err == nil && state.Sequence <= artifactSequence {
			continue
		}
		if err == nil {
			artifact.State, artifact.ReadyAt = preserveTerminalState(
				oldArtifactState, nullableInt64Ptr(oldArtifactAt), artifact.State, artifact.ReadyAt)
		}
		artifact.Sequence = state.Sequence
		if _, err := tx.Exec(`
			INSERT INTO attempt_publication_artifacts
				(attempt_id, name, path, state, ready_at, payload_key, detail, sequence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(attempt_id, name) DO UPDATE SET
				path = excluded.path, state = excluded.state, ready_at = excluded.ready_at,
				payload_key = excluded.payload_key, detail = excluded.detail,
				sequence = excluded.sequence`,
			state.AttemptID, artifact.Name, artifact.Path, artifact.State, artifact.ReadyAt,
			artifact.PayloadKey, artifact.Detail, artifact.Sequence); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func nullableInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func nullableIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// GetAttemptPublicationState returns the stored report and artifact rows for
// one attempt. An existing attempt without a report receives an explicit
// derived state, with terminal legacy attempts reporting publication facets
// as unknown rather than absent.
func GetAttemptPublicationState(database *sql.DB, attemptID int64) (*AttemptPublicationState, error) {
	state := &AttemptPublicationState{AttemptID: attemptID}
	var executionAt, requiredAt, drainAt sql.NullInt64
	var queuedItems, queuedBytes, inflightItems, inflightBytes sql.NullInt64
	var oldestAge, retainedBytes, workerLimit, workersBusy, lastProgress sql.NullInt64
	err := database.QueryRow(`
		SELECT ja.job_id, aps.sequence, aps.observed_at,
		       aps.execution_state, aps.execution_completed_at,
		       aps.required_artifacts_state, aps.required_artifacts_ready_at,
		       aps.drain_state, aps.drain_completed_at,
		       aps.queued_items, aps.queued_bytes, aps.inflight_items, aps.inflight_bytes,
		       aps.oldest_queued_age_ms, aps.retained_bytes, aps.worker_limit,
		       aps.workers_busy, aps.last_progress_at,
		       COALESCE(aps.unknown_reason, ''), COALESCE(aps.detail, '')
		FROM attempt_publication_state aps
		JOIN job_attempts ja ON ja.id = aps.attempt_id
		WHERE aps.attempt_id = ?`, attemptID).Scan(
		&state.JobID, &state.Sequence, &state.ObservedAt,
		&state.ExecutionState, &executionAt,
		&state.RequiredArtifactsState, &requiredAt,
		&state.DrainState, &drainAt,
		&queuedItems, &queuedBytes, &inflightItems, &inflightBytes,
		&oldestAge, &retainedBytes, &workerLimit, &workersBusy, &lastProgress,
		&state.UnknownReason, &state.Detail,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return unknownAttemptPublicationState(database, attemptID)
	}
	if err != nil {
		return nil, err
	}
	state.ExecutionCompletedAt = nullableInt64Ptr(executionAt)
	state.RequiredArtifactsReadyAt = nullableInt64Ptr(requiredAt)
	state.DrainCompletedAt = nullableInt64Ptr(drainAt)
	state.QueuedItems = nullableIntPtr(queuedItems)
	state.QueuedBytes = nullableInt64Ptr(queuedBytes)
	state.InflightItems = nullableIntPtr(inflightItems)
	state.InflightBytes = nullableInt64Ptr(inflightBytes)
	state.OldestQueuedAgeMS = nullableInt64Ptr(oldestAge)
	state.RetainedBytes = nullableInt64Ptr(retainedBytes)
	state.WorkerLimit = nullableIntPtr(workerLimit)
	state.WorkersBusy = nullableIntPtr(workersBusy)
	state.LastProgressAt = nullableInt64Ptr(lastProgress)

	rows, err := database.Query(`
		SELECT name, COALESCE(path, ''), state, ready_at,
		       COALESCE(payload_key, ''), COALESCE(detail, ''), sequence
		FROM attempt_publication_artifacts
		WHERE attempt_id = ? ORDER BY name`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var artifact AttemptPublicationArtifact
		var readyAt sql.NullInt64
		if err := rows.Scan(&artifact.Name, &artifact.Path, &artifact.State, &readyAt,
			&artifact.PayloadKey, &artifact.Detail, &artifact.Sequence); err != nil {
			return nil, err
		}
		artifact.ReadyAt = nullableInt64Ptr(readyAt)
		state.Artifacts = append(state.Artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return state, nil
}

func unknownAttemptPublicationState(database *sql.DB, attemptID int64) (*AttemptPublicationState, error) {
	var jobID int64
	var status string
	var endTime sql.NullInt64
	if err := database.QueryRow(`
		SELECT job_id, status, end_time FROM job_attempts WHERE id = ?`, attemptID).Scan(
		&jobID, &status, &endTime); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	state := &AttemptPublicationState{
		AttemptID:              attemptID,
		JobID:                  jobID,
		ExecutionState:         PublicationExecutionPending,
		RequiredArtifactsState: PublicationStatePending,
		DrainState:             PublicationStatePending,
	}
	if IsTerminalStatus(status) {
		state.RequiredArtifactsState = PublicationStateUnknown
		state.DrainState = PublicationStateUnknown
		state.UnknownReason = "no publication report was observed for this attempt"
		if endTime.Valid && endTime.Int64 > 0 {
			state.ExecutionState = PublicationExecutionComplete
			state.ExecutionCompletedAt = nullableInt64Ptr(endTime)
		} else {
			state.ExecutionState = PublicationExecutionUnknown
		}
	}
	return state, nil
}

func GetLatestAttemptPublicationState(database *sql.DB, jobID int64) (*AttemptPublicationState, error) {
	var attemptID sql.NullInt64
	if err := database.QueryRow(`SELECT latest_run_id FROM job_status WHERE id = ?`, jobID).Scan(&attemptID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !attemptID.Valid {
		return nil, nil
	}
	return GetAttemptPublicationState(database, attemptID.Int64)
}

type publicationReportWire struct {
	Sequence       int64 `json:"sequence"`
	ObservedAtUnix int64 `json:"observed_at_unix"`
	Facets         struct {
		ExecutionState           string `json:"execution_state"`
		ExecutionCompletedAtUnix *int64 `json:"execution_completed_at_unix"`
		RequiredArtifactsState   string `json:"required_artifacts_state"`
		RequiredArtifactsReadyAt *int64 `json:"required_artifacts_ready_at_unix"`
		DrainState               string `json:"drain_state"`
		DrainCompletedAtUnix     *int64 `json:"drain_completed_at_unix"`
		UnknownReason            string `json:"unknown_reason"`
		Detail                   string `json:"detail"`
	} `json:"facets"`
	Snapshot struct {
		QueuedItems       *int   `json:"queued_items"`
		QueuedBytes       *int64 `json:"queued_bytes"`
		InflightItems     *int   `json:"inflight_items"`
		InflightBytes     *int64 `json:"inflight_bytes"`
		OldestQueuedAgeMS *int64 `json:"oldest_queued_age_ms"`
		RetainedBytes     *int64 `json:"retained_bytes"`
		WorkerLimit       *int   `json:"worker_limit"`
		WorkersBusy       *int   `json:"workers_busy"`
		LastProgressAt    *int64 `json:"last_progress_at_unix"`
	} `json:"snapshot"`
	Artifacts []struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		State      string `json:"state"`
		ReadyAt    *int64 `json:"ready_at_unix"`
		PayloadKey string `json:"payload_key"`
		Detail     string `json:"detail"`
	} `json:"artifacts"`
}

// DecodeAttemptPublicationReport accepts either a standalone publication
// report or a completion record containing a publication field.
func DecodeAttemptPublicationReport(data []byte, jobID, attemptID int64) (*AttemptPublicationState, error) {
	var envelope struct {
		Publication *json.RawMessage `json:"publication"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse publication JSON: %w", err)
	}
	reportData := data
	if envelope.Publication != nil {
		reportData = *envelope.Publication
	}
	var report publicationReportWire
	if err := json.Unmarshal(reportData, &report); err != nil {
		return nil, fmt.Errorf("parse publication report: %w", err)
	}
	if report.Sequence <= 0 {
		return nil, errors.New("publication report is absent or has no positive sequence")
	}
	state := &AttemptPublicationState{
		AttemptID: attemptID, JobID: jobID,
		Sequence: report.Sequence, ObservedAt: report.ObservedAtUnix,
		ExecutionState:           report.Facets.ExecutionState,
		ExecutionCompletedAt:     report.Facets.ExecutionCompletedAtUnix,
		RequiredArtifactsState:   report.Facets.RequiredArtifactsState,
		RequiredArtifactsReadyAt: report.Facets.RequiredArtifactsReadyAt,
		DrainState:               report.Facets.DrainState,
		DrainCompletedAt:         report.Facets.DrainCompletedAtUnix,
		QueuedItems:              report.Snapshot.QueuedItems,
		QueuedBytes:              report.Snapshot.QueuedBytes,
		InflightItems:            report.Snapshot.InflightItems,
		InflightBytes:            report.Snapshot.InflightBytes,
		OldestQueuedAgeMS:        report.Snapshot.OldestQueuedAgeMS,
		RetainedBytes:            report.Snapshot.RetainedBytes,
		WorkerLimit:              report.Snapshot.WorkerLimit,
		WorkersBusy:              report.Snapshot.WorkersBusy,
		LastProgressAt:           report.Snapshot.LastProgressAt,
		UnknownReason:            report.Facets.UnknownReason,
		Detail:                   report.Facets.Detail,
	}
	for _, artifact := range report.Artifacts {
		state.Artifacts = append(state.Artifacts, AttemptPublicationArtifact{
			Name: artifact.Name, Path: artifact.Path, State: artifact.State,
			ReadyAt: artifact.ReadyAt, PayloadKey: artifact.PayloadKey,
			Detail: artifact.Detail, Sequence: report.Sequence,
		})
	}
	return state, nil
}

func IngestAttemptPublicationReport(database *sql.DB, data []byte, jobID, attemptID int64) (bool, error) {
	state, err := DecodeAttemptPublicationReport(data, jobID, attemptID)
	if err != nil {
		return false, err
	}
	return UpsertAttemptPublicationState(database, state)
}
