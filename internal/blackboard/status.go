package blackboard

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/r2"
)

type Store interface {
	GetObject(ctx context.Context, key string) ([]byte, error)
	ListObjects(ctx context.Context, prefix string) ([]r2.ObjectInfo, error)
}

type Status struct {
	Autopilot       *AutopilotState
	Specs           int
	Claims          int
	ActiveClaims    int
	ExpiredClaims   int
	Assignments     int
	AgentHeartbeats int
	Events          int
	ClaimErrors     []string
}

func FetchStatus(ctx context.Context, store Store, now time.Time) (*Status, error) {
	if store == nil {
		return nil, fmt.Errorf("blackboard store is required")
	}
	status := &Status{}
	if data, err := store.GetObject(ctx, controlplane.BlackboardAutopilotState()); err == nil {
		var ap AutopilotState
		if err := json.Unmarshal(data, &ap); err != nil {
			return nil, fmt.Errorf("decode autopilot blackboard state: %w", err)
		}
		status.Autopilot = &ap
	} else if !r2.IsNotFound(err) {
		return nil, err
	}

	objects, err := store.ListObjects(ctx, controlplane.BlackboardPrefix())
	if err != nil {
		return nil, err
	}
	for _, obj := range objects {
		key := obj.Key
		switch {
		case strings.HasSuffix(key, "/spec.json"):
			status.Specs++
		case strings.HasSuffix(key, "/assignment.json"):
			status.Assignments++
		case strings.HasSuffix(key, "/claim.json"):
			status.Claims++
			claim, err := fetchClaim(ctx, store, key)
			if err != nil {
				status.ClaimErrors = append(status.ClaimErrors, err.Error())
				continue
			}
			if claim.Expired(now) {
				status.ExpiredClaims++
			} else {
				status.ActiveClaims++
			}
		case strings.HasPrefix(key, controlplane.BlackboardAgentsPrefix()) && strings.HasSuffix(key, "/heartbeat.json"):
			status.AgentHeartbeats++
		case strings.HasPrefix(key, controlplane.BlackboardEventsPrefix()) && strings.HasSuffix(key, ".json"):
			status.Events++
		}
	}
	return status, nil
}

func fetchClaim(ctx context.Context, store Store, key string) (*Claim, error) {
	data, err := store.GetObject(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read claim %s: %w", key, err)
	}
	var claim Claim
	if err := json.Unmarshal(data, &claim); err != nil {
		return nil, fmt.Errorf("decode claim %s: %w", key, err)
	}
	if claim.JobID == 0 {
		claim.JobID = jobIDFromKey(key)
	}
	return &claim, nil
}

func jobIDFromKey(key string) int64 {
	parts := strings.Split(path.Clean(key), "/")
	for i, part := range parts {
		if part != "jobs" || i+1 >= len(parts) {
			continue
		}
		id, _ := strconv.ParseInt(parts[i+1], 10, 64)
		return id
	}
	return 0
}
