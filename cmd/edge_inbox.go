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
	if request.Host == "" {
		request.Host = admission.Authorization.Targets[0]
	}
	if !edgeRequestTargetAllowed(request.Host, admission.Authorization.Targets) {
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
	ack.Phase = "placed"
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
	_, editsSpend := payload.Flags["max-spend"]
	editStartsWork := payload.Action == edge.ControlEdit &&
		(payload.Flags["retry"] == "true" || payload.Flags["status"] == db.StatusQueued)
	if payload.Action == edge.ControlRestart || (editStartsWork && !editsSpend) {
		job, err := db.GetJobByID(database, payload.JobID)
		if err != nil {
			return edge.Ack{}, nil, fmt.Errorf("read retained spend request for %s: %w", jobRef, err)
		}
		if job == nil {
			return edge.Ack{}, &edge.Refusal{Code: edge.ReasonJobControlAuthority, Detail: "job-control authority check failed: job is no longer present"}, nil
		}
		if job.CLIResourceOverrides != nil && job.CLIResourceOverrides.MaxSpendCents != nil {
			requested := float64(*job.CLIResourceOverrides.MaxSpendCents) / 100
			if requested > admission.Authorization.EffectiveSpendCeilingUSD {
				return edge.Ack{}, &edge.Refusal{Code: edge.ReasonOverSpendCeiling,
					Detail: fmt.Sprintf("job-control spend check failed: job %s retains a $%.2f ceiling, above this plan's $%.2f grant",
						jobRef, requested, admission.Authorization.EffectiveSpendCeilingUSD)}, nil
			}
		}
	}

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
		err = runRestart(cmd, args)
	case edge.ControlMarkProcessed:
		err = setProcessedTag(args, true)
	case edge.ControlMarkUnprocessed:
		err = setProcessedTag(args, false)
	case edge.ControlEdit:
		err = runEdit(cmd, args)
	default:
		return edge.Ack{}, &edge.Refusal{Code: edge.ReasonMalformed, Detail: fmt.Sprintf("unknown job-control action %q", payload.Action)}, nil
	}
	if err != nil {
		return edge.Ack{}, nil, fmt.Errorf("%s %s: %w", payload.Action, jobRef, err)
	}
	return acceptedEdgeAck(admission, deps.HubHost, fmt.Sprintf("Job %s: %s completed", jobRef, payload.Action)), nil, nil
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

func pollEdgeInboxOnce(ctx context.Context, database *sql.DB, deps edgeInboxDeps) (int, error) {
	keys, err := deps.Transport.List(ctx, edge.PrefixInbox)
	if err != nil {
		return 0, fmt.Errorf("list edge inbox: %w", err)
	}
	pending := len(keys)
	for _, key := range keys {
		nonce := strings.TrimPrefix(key, edge.PrefixInbox)
		if nonce == "" || strings.Contains(nonce, "/") {
			return pending, fmt.Errorf("invalid edge inbox key %q", key)
		}
		already, err := deps.Runtime.Seen.Seen(nonce)
		if err != nil {
			return pending, fmt.Errorf("check seen nonce %s: %w", nonce, err)
		}
		if already {
			if ack, ackErr := edgeFetchAck(ctx, deps.Runtime, nonce); ackErr == nil && ack != nil {
				if err := deps.Transport.Delete(ctx, key); err != nil {
					return pending, err
				}
				pending--
				continue
			} else if ackErr != nil && !errors.Is(ackErr, edge.ErrNotFound) {
				return pending, fmt.Errorf("recover acknowledgement for %s: %w", nonce, ackErr)
			}
			jobID, found, err := db.FindJobIDByEdgeNonce(database, nonce)
			if err != nil {
				return pending, fmt.Errorf("recover admitted nonce %s: %w", nonce, err)
			}
			if !found {
				return pending, fmt.Errorf("nonce %s is recorded in the seen-set but has no job row", nonce)
			}
			job, err := db.GetJobByID(database, jobID)
			if err != nil {
				return pending, fmt.Errorf("read recovered job %s: %w", ids.FormatJobID(jobID), err)
			}
			phase := "placement_pending"
			if job != nil && job.Host != "" {
				phase = "placed"
			}
			now := time.Now().UTC()
			ack := edge.Ack{Version: 1, Nonce: nonce, Accepted: true, JobID: jobID,
				Phase: phase, HubHost: deps.HubHost, AckedAt: now, PhaseSince: now}
			if err := writeEdgeAck(ctx, deps.Transport, ack); err != nil {
				return pending, err
			}
			if err := deps.Transport.Delete(ctx, key); err != nil {
				return pending, err
			}
			pending--
			continue
		}

		object, err := deps.Transport.Get(ctx, key)
		if errors.Is(err, edge.ErrNotFound) {
			pending--
			continue
		}
		if err != nil {
			return pending, fmt.Errorf("read edge pointer %s: %w", nonce, err)
		}
		admission, refusal, err := edge.Admit(ctx, deps.Transport, object, edge.AdmitOptions{
			Verify: edge.VerifyOptions{
				Keyring: deps.Runtime.Keyring, Kinds: deps.Runtime.Kinds, Now: time.Now(),
				DefaultTTL: time.Hour, ClockSkew: time.Minute,
			},
			Policy: deps.Runtime.Policy,
			Seen:   deps.Runtime.Seen,
			ControlJobs: func(jobID int64) (string, bool, error) {
				return db.FindJobEdgeSigningKey(database, jobID)
			},
		})
		if err != nil {
			return pending, fmt.Errorf("admit edge submission %s: %w", nonce, err)
		}
		if refusal != nil {
			if err := writeEdgeAck(ctx, deps.Transport, refusalAck(nonce, deps.HubHost, refusal, time.Now().UTC())); err != nil {
				return pending, err
			}
			if !refusal.Quarantine {
				if err := deps.Transport.Delete(ctx, key); err != nil {
					return pending, err
				}
				pending--
			}
			continue
		}
		ack, handlingRefusal, err := handleEdgeAdmission(ctx, database, deps, admission)
		if err != nil {
			if rollbackErr := forgetEdgeAdmission(deps.Runtime.Seen, deps.Runtime.Kinds, admission); rollbackErr != nil {
				return pending, fmt.Errorf("handle admitted submission %s: %w; seen rollback failed: %v", nonce, err, rollbackErr)
			}
			return pending, fmt.Errorf("handle admitted submission %s: %w", nonce, err)
		}
		if handlingRefusal != nil {
			ack = refusalAck(nonce, deps.HubHost, handlingRefusal, time.Now().UTC())
		}
		if err := writeEdgeAck(ctx, deps.Transport, ack); err != nil {
			return pending, err
		}
		if err := deps.Transport.Delete(ctx, key); err != nil {
			return pending, err
		}
		pending--
		fmt.Fprintf(os.Stderr, "edge inbox handled %s (%s)\n", nonce, admission.Envelope.PayloadKind)
	}
	return pending, nil
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
