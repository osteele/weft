package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
)

var (
	graceR2List   = r2List
	graceR2Get    = r2Get
	graceR2Put    = r2Put
	graceR2Delete = r2Delete
)

type graceRequest struct {
	RequestID string
	Key       string
	Body      string
}

func drainGraceJobRequests(bucket string, instanceID int64, onPhase func(string)) ([]cloud.AgentJob, error) {
	requests, err := listGraceRequests(bucket, controlplane.GraceJobsPrefix(instanceID))
	if err != nil {
		return nil, err
	}
	var jobs []cloud.AgentJob
	for _, req := range requests {
		var payload controlplane.GraceJobsRequest
		if err := json.Unmarshal([]byte(req.Body), &payload); err != nil {
			ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandJobs, false, fmt.Sprintf("invalid jobs payload: %v", err))
			_ = graceR2Delete(bucket, req.Key)
			continue
		}
		if len(payload.Jobs) == 0 {
			ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandJobs, false, "empty jobs payload")
			_ = graceR2Delete(bucket, req.Key)
			continue
		}
		if err := applySourceUpdates(bucket, payload.Sources, onPhase); err != nil {
			ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandJobs, false, fmt.Sprintf("failed to apply sources: %v", err))
			_ = graceR2Delete(bucket, req.Key)
			continue
		}
		jobs = append(jobs, payload.Jobs...)
		ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandJobs, true, "received")
		_ = graceR2Delete(bucket, req.Key)
	}
	return jobs, nil
}

func applySourceUpdates(bucket string, updates []controlplane.SourceUpdate, onPhase func(string)) error {
	if len(updates) == 0 {
		return nil
	}
	if onPhase != nil {
		onPhase(fmt.Sprintf("sources_extracting:0/%d", len(updates)))
	}
	for i, upd := range updates {
		if strings.TrimSpace(upd.RemoteDir) == "" || strings.TrimSpace(upd.R2Key) == "" {
			return fmt.Errorf("source update %d missing remote_dir or r2_key", i+1)
		}
		if err := applySourceUpdate(bucket, upd); err != nil {
			return fmt.Errorf("apply source update %d (%s): %w", i+1, upd.RemoteDir, err)
		}
		if onPhase != nil {
			onPhase(fmt.Sprintf("sources_extracting:%d/%d", i+1, len(updates)))
		}
	}
	if onPhase != nil {
		onPhase("sources_extracted")
	}
	return nil
}

func applySourceUpdate(bucket string, upd controlplane.SourceUpdate) error {
	if err := os.MkdirAll(upd.RemoteDir, 0o755); err != nil {
		return fmt.Errorf("create remote dir %s: %w", upd.RemoteDir, err)
	}
	tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("weft-grace-source-%d.tar.gz", time.Now().UnixNano()))
	defer os.Remove(tmpPath)

	copyCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	copyCmd := exec.CommandContext(copyCtx, "rclone", "copyto", fmt.Sprintf("r2:%s/%s", bucket, upd.R2Key), tmpPath)
	copyCmd.Stderr = os.Stderr
	if err := copyCmd.Run(); err != nil {
		return fmt.Errorf("download source tarball: %w", err)
	}

	tarCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tarCmd := exec.CommandContext(tarCtx, "tar", "xzf", tmpPath, "-C", upd.RemoteDir)
	tarCmd.Stderr = os.Stderr
	if err := tarCmd.Run(); err != nil {
		return fmt.Errorf("extract source tarball: %w", err)
	}
	return nil
}

func applyGraceExtendRequests(bucket string, instanceID int64, deadline time.Time) (time.Time, error) {
	requests, err := listGraceRequests(bucket, controlplane.GraceExtendPrefix(instanceID))
	if err != nil {
		return deadline, err
	}
	for _, req := range requests {
		dur, err := time.ParseDuration(strings.TrimSpace(req.Body))
		if err != nil {
			ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandExtend, false, fmt.Sprintf("invalid duration: %v", err))
			_ = graceR2Delete(bucket, req.Key)
			continue
		}
		deadline = time.Now().Add(dur)
		ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandExtend, true, "received")
		_ = graceR2Delete(bucket, req.Key)
	}
	return deadline, nil
}

func hasGraceReleaseRequest(bucket string, instanceID int64) (bool, error) {
	requests, err := listGraceRequests(bucket, controlplane.GraceReleasePrefix(instanceID))
	if err != nil {
		return false, err
	}
	if len(requests) == 0 {
		return false, nil
	}
	for _, req := range requests {
		ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandRelease, true, "received")
		_ = graceR2Delete(bucket, req.Key)
	}
	return true, nil
}

func ackGraceRequest(bucket string, instanceID int64, requestID string, kind controlplane.GraceCommandKind, accepted bool, message string) {
	if requestID == "" {
		return
	}
	ack := controlplane.GraceCommandAck{
		RequestID:  requestID,
		Kind:       kind,
		ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Accepted:   accepted,
		Message:    message,
	}
	data, err := json.Marshal(ack)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal grace ack: %v\n", err)
		return
	}
	if err := graceR2Put(bucket, controlplane.GraceCommandAckKey(instanceID, requestID), string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "write grace ack: %v\n", err)
	}
}

func listGraceRequests(bucket, prefix string) ([]graceRequest, error) {
	names, err := graceR2List(bucket, prefix)
	if err != nil {
		return nil, err
	}
	requests := make([]graceRequest, 0, len(names))
	for _, name := range names {
		key := path.Join(prefix, name)
		body, err := graceR2Get(bucket, key)
		if err != nil {
			return nil, err
		}
		requests = append(requests, graceRequest{
			RequestID: graceRequestID(name),
			Key:       key,
			Body:      body,
		})
	}
	return requests, nil
}

func graceRequestID(name string) string {
	base := path.Base(name)
	base = strings.TrimSuffix(base, ".json")
	base = strings.TrimSuffix(base, ".txt")
	return base
}
