package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
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

func drainGraceJobRequests(bucket string, instanceID int64) ([]cloud.AgentJob, error) {
	requests, err := listGraceRequests(bucket, controlplane.GraceJobsPrefix(instanceID))
	if err != nil {
		return nil, err
	}
	var jobs []cloud.AgentJob
	for _, req := range requests {
		var payload graceJobsPayload
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
		jobs = append(jobs, payload.Jobs...)
		ackGraceRequest(bucket, instanceID, req.RequestID, controlplane.GraceCommandJobs, true, "received")
		_ = graceR2Delete(bucket, req.Key)
	}
	return jobs, nil
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
