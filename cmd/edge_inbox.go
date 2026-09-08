package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/appdirs"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/secrets"
	"github.com/spf13/cobra"
)

type edgeInboxState struct {
	LastAttempt        time.Time `json:"last_attempt"`
	LastSuccessfulPoll time.Time `json:"last_successful_poll,omitempty"`
	PendingPointers    int       `json:"pending_pointers"`
	PollError          string    `json:"poll_error,omitempty"`
}

type edgeSourceWriter interface {
	PutObject(context.Context, string, io.Reader, string) error
}

type edgeInboxDeps struct {
	Transport   edge.Transport
	Runtime     *edge.Runtime
	SourceStore edgeSourceWriter
	HubHost     string
}

func forgetEdgeNonce(seen edge.SeenSet, nonce string) error {
	forgetter, ok := seen.(interface{ Forget(string) error })
	if !ok {
		return fmt.Errorf("seen-set cannot roll back nonce %s", nonce)
	}
	return forgetter.Forget(nonce)
}

func shouldPollEdgeInbox(cfg *config.Config) bool {
	return cfg != nil && !cfg.Edge.IsEdge() && cfg.Edge.Inbound.Bucket != ""
}

func edgeInboxStatePath() (string, error) {
	dir, err := appdirs.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "edge-inbox-poller.json"), nil
}

func loadEdgeInboxState() (edgeInboxState, error) {
	path, err := edgeInboxStatePath()
	if err != nil {
		return edgeInboxState{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return edgeInboxState{}, nil
	}
	if err != nil {
		return edgeInboxState{}, fmt.Errorf("read edge inbox poller state: %w", err)
	}
	var state edgeInboxState
	if err := json.Unmarshal(data, &state); err != nil {
		return edgeInboxState{}, fmt.Errorf("decode edge inbox poller state: %w", err)
	}
	return state, nil
}

func saveEdgeInboxState(state edgeInboxState) error {
	path, err := edgeInboxStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create edge inbox poller state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode edge inbox poller state: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write edge inbox poller state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace edge inbox poller state: %w", err)
	}
	return nil
}

func writeEdgeAck(ctx context.Context, transport edge.Transport, ack edge.Ack) error {
	data, err := json.Marshal(ack)
	if err != nil {
		return fmt.Errorf("encode acknowledgement for %s: %w", ack.Nonce, err)
	}
	if err := transport.Put(ctx, edge.AckKey(ack.Nonce), data); err != nil {
		return fmt.Errorf("write acknowledgement for %s: %w", ack.Nonce, err)
	}
	return nil
}

func refusalAck(nonce, hubHost string, refusal *edge.Refusal, now time.Time) edge.Ack {
	phase := "admission"
	if refusal.IsAuthenticationFailure() {
		phase = "authentication"
	} else if refusal.IsAuthorizationFailure() {
		phase = "authorization"
	} else if refusal.Code == edge.ReasonActionFailed {
		phase = "execution"
	}
	return edge.Ack{
		Version: 1, Nonce: nonce, Accepted: false,
		ReasonCode: string(refusal.Code), Detail: refusal.Detail,
		Phase: phase, HubHost: hubHost, AckedAt: now, PhaseSince: now,
	}
}

func prepareEdgeSource(params *ops.QueueJobParams, body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("admitted edge job has no source closure")
	}
	if params.Metadata == nil || params.Metadata.Source == nil || params.Metadata.Source.Pin == nil || len(params.Metadata.Source.Pin.Roots) != 1 {
		return "", fmt.Errorf("admitted edge job must name exactly one pinned source root")
	}
	root := &params.Metadata.Source.Pin.Roots[0]
	if len(root.Hash) != sha256.Size*2 {
		return "", fmt.Errorf("admitted edge job source root has invalid canonical hash %q", root.Hash)
	}
	if _, err := hex.DecodeString(root.Hash); err != nil {
		return "", fmt.Errorf("admitted edge job source root has invalid canonical hash %q: %w", root.Hash, err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("admitted edge source closure is not gzip: %w", err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, reader)
	closeErr := reader.Close()
	if copyErr != nil {
		if closeErr != nil {
			return "", fmt.Errorf("read admitted edge source closure: %w; close failed: %v", copyErr, closeErr)
		}
		return "", fmt.Errorf("read admitted edge source closure: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close admitted edge source closure: %w", closeErr)
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != root.Hash {
		return "", fmt.Errorf("admitted edge source closure canonical hash is %s, want %s", actual, root.Hash)
	}
	key := dataplane.SourceTarballV2(root.Hash)
	root.R2Key = key
	if len(params.Metadata.Source.Roots) == 1 {
		params.Metadata.Source.Roots[0].R2Key = key
	}
	return key, nil
}

func installEdgeSource(ctx context.Context, store edgeSourceWriter, key string, body []byte) error {
	if store == nil {
		return fmt.Errorf("source object store is not configured")
	}
	return store.PutObject(ctx, key, bytes.NewReader(body), "application/gzip")
}

func edgeRequestTargetAllowed(host string, targets []string) bool {
	for _, target := range targets {
		if strings.EqualFold(host, target) {
			return true
		}
	}
	return false
}

func decodeEdgeRunRequest(data []byte) (ops.QueueJobParams, error) {
	var request ops.QueueJobParams
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return ops.QueueJobParams{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return ops.QueueJobParams{}, fmt.Errorf("job request contains a second JSON value")
		}
		return ops.QueueJobParams{}, err
	}
	return request, nil
}

func forgetEdgeAdmission(seen edge.SeenSet, kinds *edge.KindRegistry, admission *edge.Admission) error {
	if err := forgetEdgeNonce(seen, admission.Envelope.Nonce); err != nil {
		return err
	}
	policy, ok := kinds.Lookup(admission.Envelope.PayloadKind)
	if !ok || !policy.ContentIdempotent {
		return nil
	}
	return forgetEdgeNonce(seen, edge.ContentSeenKey(admission.KeyID, admission.Envelope.PayloadKind, admission.Envelope.PayloadDigest))
}

func handleEdgeAdmission(ctx context.Context, database *sql.DB, deps edgeInboxDeps, admission *edge.Admission) (edge.Ack, *edge.Refusal, error) {
	if admission == nil {
		return edge.Ack{}, nil, fmt.Errorf("admission is nil")
	}
	switch admission.Envelope.PayloadKind {
	case edge.KindWeftJobSubmission:
		return handleEdgeJobSubmission(ctx, database, deps, admission)
	case edge.KindWeftJobControl:
		return handleEdgeJobControl(database, deps, admission)
	case edge.KindWeftBugReport:
		return handleEdgeBugReport(deps, admission)
	case edge.KindPlanEnded:
		return acceptedEdgeAck(admission, deps.HubHost, "Plan authority ended"), nil, nil
	default:
		return edge.Ack{}, nil, fmt.Errorf("admitted payload kind %q has no hub handler", admission.Envelope.PayloadKind)
	}
}

func acceptedEdgeAck(admission *edge.Admission, hubHost, detail string) edge.Ack {
	now := time.Now().UTC()
	return edge.Ack{
		Version: 1, Nonce: admission.Envelope.Nonce, Accepted: true, Detail: detail,
		Phase: "performed", HubHost: hubHost, AckedAt: now, PhaseSince: now,
	}
}

func handleEdgeJobSubmission(ctx context.Context, database *sql.DB, deps edgeInboxDeps, admission *edge.Admission) (edge.Ack, *edge.Refusal, error) {
	if admission.Job == nil {
		return edge.Ack{}, nil, fmt.Errorf("job submission admitted without a job payload")
	}
	request, err := decodeEdgeRunRequest(admission.Job.QueueParams)
	if err != nil {
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonMalformed, Detail: fmt.Sprintf("job request is not valid: %v", err)}, nil
	}
	if request.Command != admission.Job.Command || request.WorkingDir != admission.Job.WorkingDir || request.Project != admission.Job.Project {
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonMalformed, Detail: "job request fields do not match the authenticated payload"}, nil
	}
	if request.Host != "" && !edgeRequestTargetAllowed(request.Host, admission.Authorization.Targets) {
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonTargetNotAllowed, Detail: fmt.Sprintf("authenticated job request target %q is outside the authorized targets %v", request.Host, admission.Authorization.Targets)}, nil
	}
	sourceKey, err := prepareEdgeSource(&request, admission.SourceClosure)
	if err != nil {
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonPayloadMismatch, Detail: err.Error()}, nil
	}
	if err := installEdgeSource(ctx, deps.SourceStore, sourceKey, admission.SourceClosure); err != nil {
		return edge.Ack{}, nil, fmt.Errorf("install source closure: %w", err)
	}
	if request.CLIOverrides == nil {
		request.CLIOverrides = &db.CLIResourceOverrides{}
	}
	ceilingCents := int(math.Round(admission.Authorization.EffectiveSpendCeilingUSD * 100))
	request.CLIOverrides.MaxSpendCents = &ceilingCents
	provenance := &db.EdgeSubmissionProvenance{
		SubmitterHost: admission.SigningHost, SigningKeyID: admission.KeyID,
		DeploymentSourceDigest: admission.Envelope.DeploymentSourceDigest, Nonce: admission.Envelope.Nonce,
		AuthorizedTargets: append([]string(nil), admission.Authorization.Targets...),
	}
	jobID, err := recordAndPlaceRunRequest(ctx, database, request, provenance,
		func(ctx context.Context, database *sql.DB, request ops.QueueJobParams) (int64, error) {
			return ops.RecordQueuedJobContext(ctx, database, request)
		})
	if err != nil {
		return edge.Ack{}, nil, fmt.Errorf("record admitted job: %w", err)
	}
	ack := acceptedEdgeAck(admission, deps.HubHost, ids.FormatJobID(jobID))
	ack.JobID = jobID
	ack.Phase = "placement_pending"
	if request.Host != "" {
		ack.Phase = "placed"
	}
	return ack, nil, nil
}

func controlCommand(payload *edge.WeftJobControlPayload) (*cobra.Command, error) {
	cmd := &cobra.Command{Use: string(payload.Action)}
	switch payload.Action {
	case edge.ControlRestart:
		addRestartFlags(cmd)
	case edge.ControlEdit:
		addEditFlags(cmd)
	}
	for name, value := range payload.Flags {
		if cmd.Flags().Lookup(name) == nil {
			return nil, fmt.Errorf("action %s does not accept flag --%s", payload.Action, name)
		}
		if err := cmd.Flags().Set(name, value); err != nil {
			return nil, fmt.Errorf("set --%s: %w", name, err)
		}
	}
	return cmd, nil
}

func handleEdgeJobControl(database *sql.DB, deps edgeInboxDeps, admission *edge.Admission) (edge.Ack, *edge.Refusal, error) {
	payload := admission.Control
	if payload == nil {
		return edge.Ack{}, nil, fmt.Errorf("job control admitted without a control payload")
	}
	cmd, err := controlCommand(payload)
	if err != nil {
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonMalformed, Detail: err.Error()}, nil
	}
	jobRef := ids.FormatJobID(payload.JobID)
	job, err := db.GetJobByID(database, payload.JobID)
	if err != nil {
		return edge.Ack{}, nil, fmt.Errorf("read job-control target %s: %w", jobRef, err)
	}
	if job == nil {
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonActionFailed, Detail: fmt.Sprintf("job %s not found", jobRef)}, nil
	}
	if refusal := edgeJobControlStateRefusal(payload, job); refusal != nil {
		return edge.Ack{}, refusal, nil
	}
	// A control that names no spend ceiling may narrow the stored ceiling to the
	// admitted grant, but it cannot widen an existing lower ceiling. An explicit
	// edit --max-spend installs its admitted value, including the plan grant when
	// the edge requested zero.
	spendCeilingCents := edgeControlSpendCeilingOverride(payload, job, int(math.Round(admission.Authorization.EffectiveSpendCeilingUSD*100)))

	previous := activeEdgeSubmit
	activeEdgeSubmit = nil
	defer func() { activeEdgeSubmit = previous }()
	args := []string{jobRef}
	switch payload.Action {
	case edge.ControlCancel:
		err = runCancelWithParser(cmd, args, ParseJobIDs)
	case edge.ControlKill:
		err = runKill(cmd, args)
	case edge.ControlPause:
		err = runPause(cmd, args)
	case edge.ControlResume:
		err = runResume(cmd, args)
	case edge.ControlUnpause:
		err = runUnpause(cmd, args)
	case edge.ControlRestart:
		err = runRestartWithSpendCeiling(cmd, args, spendCeilingCents)
	case edge.ControlMarkProcessed:
		err = setProcessedTag(args, true)
	case edge.ControlMarkUnprocessed:
		err = setProcessedTag(args, false)
	case edge.ControlEdit:
		err = runEditWithSpendCeiling(cmd, args, spendCeilingCents)
	default:
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonMalformed, Detail: fmt.Sprintf("unknown job-control action %q", payload.Action)}, nil
	}
	if err != nil {
		var stateDecision *jobStateDecisionError
		if isUsageError(err) || errors.Is(err, errFlagConflict) || errors.Is(err, db.ErrJobNotFound) || errors.As(err, &stateDecision) {
			return edge.Ack{}, &edge.Refusal{Code: edge.ReasonActionFailed, Detail: fmt.Sprintf("%s %s: %v", payload.Action, jobRef, err)}, nil
		}
		return edge.Ack{}, nil, fmt.Errorf("%s %s: %w", payload.Action, jobRef, err)
	}
	return acceptedEdgeAck(admission, deps.HubHost, fmt.Sprintf("Job %s: %s completed", jobRef, payload.Action)), nil, nil
}

func edgeControlSpendCeilingOverride(payload *edge.WeftJobControlPayload, job *db.Job, admittedCents int) *int {
	if payload.Action == edge.ControlEdit {
		if _, explicitlyRequested := payload.Flags["max-spend"]; explicitlyRequested {
			return &admittedCents
		}
	}
	if job.CLIResourceOverrides != nil && job.CLIResourceOverrides.MaxSpendCents != nil &&
		*job.CLIResourceOverrides.MaxSpendCents <= admittedCents {
		return nil
	}
	return &admittedCents
}

func edgeJobControlStateRefusal(payload *edge.WeftJobControlPayload, job *db.Job) *edge.Refusal {
	status := job.EffectiveStatus()
	detail := ""
	switch payload.Action {
	case edge.ControlPause:
		if job.Backend == db.BackendSkyPilot {
			detail = fmt.Sprintf("job %s is owned by SkyPilot and cannot be marked draft", ids.FormatJobID(job.ID))
		}
	case edge.ControlResume:
		if job.IsRentalJob() {
			detail = fmt.Sprintf("job %s: resume is not supported for rental jobs", ids.FormatJobID(job.ID))
		} else if status != db.StatusPaused {
			detail = fmt.Sprintf("job %s is %s; only paused jobs can be resumed", ids.FormatJobID(job.ID), status)
		}
	case edge.ControlUnpause:
		if job.Backend == db.BackendSkyPilot && status == db.StatusDraft {
			detail = fmt.Sprintf("cannot change status of SkyPilot job %s through Weft's local execution controls", ids.FormatJobID(job.ID))
		}
	case edge.ControlRestart:
		if job.Backend == db.BackendSkyPilot {
			detail = fmt.Sprintf("SkyPilot job %s cannot be restarted by Weft; submit a new external job instead", ids.FormatJobID(job.ID))
		} else if status == db.StatusRunning || status == db.StatusStarting {
			detail = fmt.Sprintf("job %s is currently %s; kill it first if you want to retry", ids.FormatJobID(job.ID), status)
		}
	case edge.ControlEdit:
		if job.Backend == db.BackendSkyPilot {
			detail = fmt.Sprintf("SkyPilot job %s cannot be edited through Weft; change executor-owned fields in SkyPilot", ids.FormatJobID(job.ID))
		} else if startsWork := payload.Flags["retry"] == "true" || payload.Flags["status"] == db.StatusQueued; startsWork && status != db.StatusQueued && !requeueableStatuses[status] {
			detail = fmt.Sprintf("cannot change job %s from '%s' to 'queued'; only killed/dead/failed/canceled jobs can be requeued", ids.FormatJobID(job.ID), status)
		}
	case edge.ControlCancel, edge.ControlKill:
		if db.IsTerminalStatus(status) {
			detail = fmt.Sprintf("job %s is %s; nothing to %s", ids.FormatJobID(job.ID), status, payload.Action)
		}
	}
	if detail == "" {
		return nil
	}
	return &edge.Refusal{Code: edge.ReasonActionFailed, Detail: detail}
}

func handleEdgeBugReport(deps edgeInboxDeps, admission *edge.Admission) (edge.Ack, *edge.Refusal, error) {
	payload := admission.BugReport
	if payload == nil {
		return edge.Ack{}, nil, fmt.Errorf("bug record admitted without a bug-report payload")
	}
	database, err := db.OpenBugDB()
	if err != nil {
		return edge.Ack{}, nil, err
	}
	defer database.Close()
	switch payload.Action {
	case edge.BugReportCreate:
		bug, created, err := db.ReportBug(database, db.BugReport{
			Title: payload.Title, Kind: payload.Kind, Scope: payload.Scope,
			Likelihood: payload.Likelihood, Severity: payload.Severity,
			Fingerprint: payload.Fingerprint, JobID: payload.JobID, Host: payload.Host,
			Summary: payload.Summary, Detail: payload.Detail, Note: payload.Note,
		})
		if err != nil {
			if db.IsBugFingerprintClosed(err) {
				return edge.Ack{}, &edge.Refusal{Code: edge.ReasonActionFailed, Detail: err.Error()}, nil
			}
			return edge.Ack{}, nil, err
		}
		verb := "Reported"
		if !created {
			verb = "Updated"
		}
		return acceptedEdgeAck(admission, deps.HubHost, fmt.Sprintf("%s %s: %s", verb, db.FormatBugID(bug.ID), bug.Title)), nil, nil
	case edge.BugReportNote:
		bugID, err := db.ParseBugID(payload.BugID)
		if err != nil {
			return edge.Ack{}, &edge.Refusal{Code: edge.ReasonMalformed, Detail: err.Error()}, nil
		}
		if err := db.AddBugNote(database, bugID, payload.Note); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return edge.Ack{}, &edge.Refusal{Code: edge.ReasonActionFailed, Detail: fmt.Sprintf("bug %s not found", db.FormatBugID(bugID))}, nil
			}
			return edge.Ack{}, nil, err
		}
		return acceptedEdgeAck(admission, deps.HubHost, fmt.Sprintf("Added note to %s", db.FormatBugID(bugID))), nil, nil
	default:
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonMalformed, Detail: fmt.Sprintf("unknown bug-report action %q", payload.Action)}, nil
	}
}

func edgeVerifyOptions(runtime *edge.Runtime) edge.VerifyOptions {
	return edge.VerifyOptions{
		Keyring: runtime.Keyring, Kinds: runtime.Kinds, Now: time.Now(),
		DefaultTTL: time.Hour, ClockSkew: time.Minute,
	}
}

func recoverSeenEdgePointer(ctx context.Context, database *sql.DB, deps edgeInboxDeps, key, nonce string, object []byte) (bool, error) {
	if ack, err := edgeFetchAck(ctx, deps.Runtime, nonce); err == nil && ack != nil {
		if err := deps.Transport.Delete(ctx, key); err != nil {
			return false, fmt.Errorf("delete acknowledged edge pointer %s: %w", nonce, err)
		}
		return true, nil
	} else if err != nil && !errors.Is(err, edge.ErrNotFound) {
		return false, fmt.Errorf("recover acknowledgement for %s: %w", nonce, err)
	}

	verified, refusal, err := edge.VerifyRecorded(object, edgeVerifyOptions(deps.Runtime))
	if err != nil {
		return false, fmt.Errorf("authenticate recorded edge pointer %s: %w", nonce, err)
	}
	if refusal != nil {
		return false, fmt.Errorf("authenticate recorded edge pointer %s: %s", nonce, refusal.Detail)
	}
	envelope := verified.Envelope()
	if envelope.Nonce != nonce {
		return false, fmt.Errorf("recorded edge pointer key nonce %s does not match authenticated envelope nonce %s", nonce, envelope.Nonce)
	}

	switch envelope.PayloadKind {
	case edge.KindWeftJobSubmission:
		jobID, found, err := db.FindJobIDByEdgeNonce(database, nonce)
		if err != nil {
			return false, fmt.Errorf("recover admitted nonce %s: %w", nonce, err)
		}
		if !found {
			if err := forgetEdgeNonce(deps.Runtime.Seen, nonce); err != nil {
				return false, fmt.Errorf("nonce %s has no job row and could not be rolled back: %w", nonce, err)
			}
			return false, fmt.Errorf("nonce %s has no job row; rolled back admission for retry", nonce)
		}
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return false, fmt.Errorf("read recovered job %s: %w", ids.FormatJobID(jobID), err)
		}
		phase := "placement_pending"
		if job != nil && job.Host != "" {
			phase = "placed"
		}
		now := time.Now().UTC()
		ack := edge.Ack{Version: 1, Nonce: nonce, Accepted: true, JobID: jobID,
			Phase: phase, HubHost: deps.HubHost, AckedAt: now, PhaseSince: now}
		if err := writeEdgeAck(ctx, deps.Transport, ack); err != nil {
			return false, err
		}
	case edge.KindWeftJobControl, edge.KindWeftBugReport, edge.KindPlanEnded:
		admission := &edge.Admission{Envelope: envelope}
		ack := acceptedEdgeAck(admission, deps.HubHost, "Submission accepted; original action detail is unavailable")
		if err := writeEdgeAck(ctx, deps.Transport, ack); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("recorded payload kind %q has no recovery policy", envelope.PayloadKind)
	}
	if err := deps.Transport.Delete(ctx, key); err != nil {
		return false, fmt.Errorf("delete recovered edge pointer %s: %w", nonce, err)
	}
	return true, nil
}

func processEdgeInboxPointer(ctx context.Context, database *sql.DB, deps edgeInboxDeps, key string) (bool, error) {
	nonce := strings.TrimPrefix(key, edge.PrefixInbox)
	if nonce == "" || strings.Contains(nonce, "/") {
		return false, fmt.Errorf("invalid edge inbox key %q", key)
	}
	object, err := deps.Transport.Get(ctx, key)
	if errors.Is(err, edge.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read edge pointer %s: %w", nonce, err)
	}
	already, err := deps.Runtime.Seen.Seen(nonce)
	if err != nil {
		return false, fmt.Errorf("check seen nonce %s: %w", nonce, err)
	}
	if already {
		return recoverSeenEdgePointer(ctx, database, deps, key, nonce, object)
	}

	admission, refusal, err := edge.Admit(ctx, deps.Transport, object, edge.AdmitOptions{
		Verify: edgeVerifyOptions(deps.Runtime),
		Policy: deps.Runtime.Policy,
		Seen:   deps.Runtime.Seen,
		ControlJobs: func(jobID int64) (string, bool, error) {
			return db.FindJobEdgeSigningKey(database, jobID)
		},
	})
	if err != nil {
		return false, fmt.Errorf("admit edge submission %s: %w", nonce, err)
	}
	if refusal != nil {
		if err := writeEdgeAck(ctx, deps.Transport, refusalAck(nonce, deps.HubHost, refusal, time.Now().UTC())); err != nil {
			return false, err
		}
		if refusal.Quarantine {
			return false, nil
		}
		if err := deps.Transport.Delete(ctx, key); err != nil {
			return false, err
		}
		return true, nil
	}
	ack, handlingRefusal, err := handleEdgeAdmission(ctx, database, deps, admission)
	if err != nil {
		if rollbackErr := forgetEdgeAdmission(deps.Runtime.Seen, deps.Runtime.Kinds, admission); rollbackErr != nil {
			return false, fmt.Errorf("handle admitted submission %s: %w; seen rollback failed: %v", nonce, err, rollbackErr)
		}
		return false, fmt.Errorf("handle admitted submission %s: %w", nonce, err)
	}
	if handlingRefusal != nil {
		ack = refusalAck(nonce, deps.HubHost, handlingRefusal, time.Now().UTC())
	}
	if err := writeEdgeAck(ctx, deps.Transport, ack); err != nil {
		return false, err
	}
	if err := deps.Transport.Delete(ctx, key); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "edge inbox handled %s (%s)\n", nonce, admission.Envelope.PayloadKind)
	return true, nil
}

func pollEdgeInboxOnce(ctx context.Context, database *sql.DB, deps edgeInboxDeps) (int, error) {
	keys, err := deps.Transport.List(ctx, edge.PrefixInbox)
	if err != nil {
		return 0, fmt.Errorf("list edge inbox: %w", err)
	}
	pending := len(keys)
	var keyErrors []error
	for _, key := range keys {
		drained, err := processEdgeInboxPointer(ctx, database, deps, key)
		if err != nil {
			keyErrors = append(keyErrors, fmt.Errorf("edge inbox key %s: %w", key, err))
			continue
		}
		if drained {
			pending--
		}
	}
	return pending, errors.Join(keyErrors...)
}

func runEdgeInboxPoller(ctx context.Context, database *sql.DB, cfg *config.Config) {
	runtime, _, err := edgeRuntime("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: edge inbox poller unavailable: %s\n", secrets.RedactText(err.Error()))
		return
	}
	sourceStore, err := newR2ClientFromConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: edge inbox poller source store unavailable: %s\n", secrets.RedactText(err.Error()))
		return
	}
	hostname, err := os.Hostname()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: edge inbox poller cannot determine hub host: %s\n", err)
		return
	}
	deps := edgeInboxDeps{Transport: runtime.Transport, Runtime: runtime, SourceStore: sourceStore, HubHost: hostname}
	poll := func() {
		attempt := time.Now().UTC()
		pending, pollErr := pollEdgeInboxOnce(ctx, database, deps)
		state, stateErr := loadEdgeInboxState()
		if stateErr != nil {
			fmt.Fprintf(os.Stderr, "warning: load edge inbox poller state: %s\n", stateErr)
		}
		state.LastAttempt = attempt
		if pollErr != nil {
			state.PollError = secrets.RedactText(pollErr.Error())
			fmt.Fprintf(os.Stderr, "warning: poll edge inbox: %s\n", state.PollError)
		} else {
			state.LastSuccessfulPoll = time.Now().UTC()
			state.PendingPointers = pending
			state.PollError = ""
		}
		if err := saveEdgeInboxState(state); err != nil {
			fmt.Fprintf(os.Stderr, "warning: save edge inbox poller state: %s\n", err)
		}
	}
	poll()
	ticker := time.NewTicker(cfg.Edge.PollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
