package localmutate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/daemonapi"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

const (
	OpSubmitJob       = "submit_job"
	OpSetJobTag       = "set_job_tag"
	defaultRPCTimeout = 60 * time.Second
	submitRPCTimeout  = 2 * time.Second
)

type SetJobTagRequest struct {
	JobID   int64  `json:"job_id"`
	Tag     string `json:"tag"`
	Present bool   `json:"present"`
}

var (
	daemonPathsFunc           = daemoncontrol.DefaultPaths
	daemonStatusFunc          = daemoncontrol.CurrentStatus
	dialMutationFunc          = daemonapi.DialMutation
	dialSubmitFunc            = daemonapi.DialSubmitJob
	recordQueuedJobDirectFunc = ops.RecordQueuedJob
)

func Handler(ctx context.Context, database *sql.DB, req daemonapi.MutationRequest) (json.RawMessage, error) {
	switch req.Op {
	case OpSubmitJob:
		var params ops.QueueJobParams
		if err := json.Unmarshal(req.Payload, &params); err != nil {
			return nil, fmt.Errorf("decode %s payload: %w", req.Op, err)
		}
		jobID, err := ops.RecordQueuedJobContext(ctx, database, params)
		if err != nil {
			return nil, err
		}
		return json.Marshal(daemonapi.SubmitJobResult{JobID: jobID})
	case OpSetJobTag:
		var payload SetJobTagRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode %s payload: %w", req.Op, err)
		}
		if err := setJobTagDirect(database, payload.JobID, payload.Tag, payload.Present); err != nil {
			return nil, err
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported mutation op %q", req.Op)
	}
}

func RecordQueuedJob(ctx context.Context, database *sql.DB, params ops.QueueJobParams) (int64, error) {
	var result daemonapi.SubmitJobResult
	used, err := tryDaemonMutationWithTimeout(ctx, OpSubmitJob, params, &result, submitRPCTimeout)
	if used {
		if err != nil {
			if jobID, ok := findSubmittedJobByToken(database, params.SubmitToken); ok {
				return jobID, nil
			}
			if strings.TrimSpace(params.SubmitToken) != "" {
				jobID, directErr := recordQueuedJobDirectFunc(database, params)
				if directErr == nil {
					return jobID, nil
				}
				if jobID, ok := findSubmittedJobByToken(database, params.SubmitToken); ok {
					return jobID, nil
				}
				return 0, fmt.Errorf("submit via daemon failed: %w; direct fallback failed: %v", err, directErr)
			}
		}
		return result.JobID, err
	}
	if jobID, used, err := tryLegacyDaemonSubmit(ctx, params); used {
		return jobID, err
	}
	return ops.RecordQueuedJob(database, params)
}

func findSubmittedJobByToken(database *sql.DB, token string) (int64, bool) {
	jobID, ok, err := db.FindJobIDBySubmitToken(database, token)
	if err != nil {
		return 0, false
	}
	return jobID, ok
}

func SetProcessedTag(ctx context.Context, database *sql.DB, jobID int64, processed bool) error {
	return SetJobTag(ctx, database, jobID, db.ProcessedTag, processed)
}

func SetJobTag(ctx context.Context, database *sql.DB, jobID int64, tag string, present bool) error {
	payload := SetJobTagRequest{JobID: jobID, Tag: tag, Present: present}
	used, err := tryDaemonMutation(ctx, OpSetJobTag, payload, nil)
	if used {
		return err
	}
	return setJobTagDirect(database, jobID, tag, present)
}

func setJobTagDirect(database *sql.DB, jobID int64, tag string, present bool) error {
	if present {
		return db.AddJobTag(database, jobID, tag)
	}
	return db.RemoveJobTag(database, jobID, tag)
}

func tryDaemonMutation(ctx context.Context, op string, payload any, result any) (bool, error) {
	return tryDaemonMutationWithTimeout(ctx, op, payload, result, defaultRPCTimeout)
}

func tryDaemonMutationWithTimeout(ctx context.Context, op string, payload any, result any, timeout time.Duration) (bool, error) {
	paths := daemonPathsFunc()
	status, err := daemonStatusFunc(paths)
	if err != nil || !status.Live {
		return false, nil
	}
	callCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()
	err = dialMutationFunc(callCtx, paths.SocketFile, op, payload, result)
	if err == nil {
		return true, nil
	}
	if daemonMutationUnsupported(err) {
		return false, nil
	}
	return true, err
}

func tryLegacyDaemonSubmit(ctx context.Context, params ops.QueueJobParams) (int64, bool, error) {
	paths := daemonPathsFunc()
	status, err := daemonStatusFunc(paths)
	if err != nil || !status.Live {
		return 0, false, nil
	}
	callCtx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	jobID, err := dialSubmitFunc(callCtx, paths.SocketFile, params)
	if err == nil {
		return jobID, true, nil
	}
	if strings.Contains(err.Error(), "unsupported request type") {
		return 0, false, nil
	}
	return 0, true, err
}

func daemonMutationUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, `unsupported request type "mutate"`) ||
		strings.Contains(msg, "mutation API unavailable")
}

func withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return withTimeout(ctx, defaultRPCTimeout)
}

func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}
