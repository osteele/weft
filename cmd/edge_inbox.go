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
		if admission == nil || admission.Job == nil {
			return pending, fmt.Errorf("submission %s admitted without a job payload", nonce)
		}
		request, err := decodeEdgeRunRequest(admission.Job.QueueParams)
		if err != nil {
			refusal := &edge.Refusal{Code: edge.ReasonMalformed, Detail: fmt.Sprintf("job request is not valid: %v", err)}
			if err := writeEdgeAck(ctx, deps.Transport, refusalAck(nonce, deps.HubHost, refusal, time.Now().UTC())); err != nil {
				return pending, err
			}
			if err := deps.Transport.Delete(ctx, key); err != nil {
				return pending, err
			}
			pending--
			continue
		}
		if request.Command != admission.Job.Command || request.WorkingDir != admission.Job.WorkingDir || request.Project != admission.Job.Project {
			refusal := &edge.Refusal{Code: edge.ReasonMalformed, Detail: "job request fields do not match the authenticated payload"}
			if err := writeEdgeAck(ctx, deps.Transport, refusalAck(nonce, deps.HubHost, refusal, time.Now().UTC())); err != nil {
				return pending, err
			}
			if err := deps.Transport.Delete(ctx, key); err != nil {
				return pending, err
			}
			pending--
			continue
		}
		if request.Host == "" {
			request.Host = admission.Authorization.Targets[0]
		}
		if !edgeRequestTargetAllowed(request.Host, admission.Authorization.Targets) {
			refusal := &edge.Refusal{Code: edge.ReasonTargetNotAllowed, Detail: fmt.Sprintf("authenticated job request target %q is outside the authorized targets %v", request.Host, admission.Authorization.Targets)}
			if err := writeEdgeAck(ctx, deps.Transport, refusalAck(nonce, deps.HubHost, refusal, time.Now().UTC())); err != nil {
				return pending, err
			}
			if err := deps.Transport.Delete(ctx, key); err != nil {
				return pending, err
			}
			pending--
			continue
		}
		sourceKey, err := prepareEdgeSource(&request, admission.SourceClosure)
		if err != nil {
			refusal := &edge.Refusal{Code: edge.ReasonPayloadMismatch, Detail: err.Error()}
			if err := writeEdgeAck(ctx, deps.Transport, refusalAck(nonce, deps.HubHost, refusal, time.Now().UTC())); err != nil {
				return pending, err
			}
			if err := deps.Transport.Delete(ctx, key); err != nil {
				return pending, err
			}
			pending--
			continue
		}
		if err := installEdgeSource(ctx, deps.SourceStore, sourceKey, admission.SourceClosure); err != nil {
			if rollbackErr := forgetEdgeNonce(deps.Runtime.Seen, nonce); rollbackErr != nil {
				return pending, fmt.Errorf("install source closure for %s: %w; seen rollback failed: %v", nonce, err, rollbackErr)
			}
			return pending, fmt.Errorf("install source closure for %s: %w", nonce, err)
		}
		if request.CLIOverrides == nil {
			request.CLIOverrides = &db.CLIResourceOverrides{}
		}
		ceilingCents := int(math.Round(admission.Authorization.EffectiveSpendCeilingUSD * 100))
		request.CLIOverrides.MaxSpendCents = &ceilingCents
		provenance := &db.EdgeSubmissionProvenance{
			SubmitterHost: admission.SigningHost, SigningKeyID: admission.KeyID,
			DeploymentSourceDigest: admission.Envelope.DeploymentSourceDigest, Nonce: nonce,
		}
		jobID, err := recordAndPlaceRunRequest(ctx, database, request, provenance,
			func(ctx context.Context, database *sql.DB, request ops.QueueJobParams) (int64, error) {
				return ops.RecordQueuedJobContext(ctx, database, request)
			})
		if err != nil {
			if rollbackErr := forgetEdgeNonce(deps.Runtime.Seen, nonce); rollbackErr != nil {
				return pending, fmt.Errorf("record admitted submission %s: %w; seen rollback failed: %v", nonce, err, rollbackErr)
			}
			return pending, fmt.Errorf("record admitted submission %s: %w", nonce, err)
		}
		now := time.Now().UTC()
		admitted := edge.Ack{Version: 1, Nonce: nonce, Accepted: true, JobID: jobID,
			Phase: "admitted", HubHost: deps.HubHost, AckedAt: now, PhaseSince: now}
		if err := writeEdgeAck(ctx, deps.Transport, admitted); err != nil {
			return pending, err
		}
		placement := admitted
		placement.Phase = "placed"
		placement.AckedAt = time.Now().UTC()
		placement.PhaseSince = placement.AckedAt
		if err := writeEdgeAck(ctx, deps.Transport, placement); err != nil {
			return pending, err
		}
		if err := deps.Transport.Delete(ctx, key); err != nil {
			return pending, err
		}
		pending--
		fmt.Fprintf(os.Stderr, "edge inbox admitted %s as %s\n", nonce, ids.FormatJobID(jobID))
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
