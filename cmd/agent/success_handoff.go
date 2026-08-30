package main

import (
	"fmt"
	"os"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

var successHandoffPollInterval = 5 * time.Second

type successHandoffWaitConfig struct {
	InstanceID int64
	R2Bucket   string
	PhaseKey   string
	MaxTime    time.Duration
	StartTime  time.Time
	OnPhase    func(string)
}

// successHandoffWait waits only when a durable, unexpired lease exists. It
// returns jobs submitted by the normal reuse path; an empty result means the
// lease expired, was released, or never existed and the caller may terminate.
func successHandoffWait(cfg successHandoffWaitConfig) []cloud.AgentJob {
	deadline, err := activeSuccessHandoffDeadline(cfg.R2Bucket, cfg.InstanceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read success handoff lease: %v\n", err)
		return nil
	}
	if deadline.IsZero() {
		return nil
	}
	if cfg.MaxTime > 0 {
		budgetDeadline := cfg.StartTime.Add(cfg.MaxTime)
		if budgetDeadline.Before(deadline) {
			deadline = budgetDeadline
		}
	}
	if !deadline.After(time.Now()) {
		return nil
	}
	recordPhase(cfg.R2Bucket, cfg.PhaseKey, "handoff", 0, cfg.OnPhase)
	fmt.Printf("Jobs completed. Holding rental for scheduler handoff until %s.\n", deadline.UTC().Format(time.RFC3339))

	ticker := time.NewTicker(successHandoffPollInterval)
	defer ticker.Stop()
	for {
		if jobs := checkForNewJobs(cfg.R2Bucket, cfg.InstanceID, cfg.OnPhase); len(jobs) > 0 {
			return jobs
		}
		if released, err := hasGraceReleaseRequest(cfg.R2Bucket, cfg.InstanceID); err != nil {
			fmt.Fprintf(os.Stderr, "read handoff release: %v\n", err)
		} else if released {
			return nil
		}
		latest, err := activeSuccessHandoffDeadline(cfg.R2Bucket, cfg.InstanceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "refresh success handoff lease: %v\n", err)
		} else if latest.After(deadline) {
			deadline = latest
			if cfg.MaxTime > 0 && cfg.StartTime.Add(cfg.MaxTime).Before(deadline) {
				deadline = cfg.StartTime.Add(cfg.MaxTime)
			}
		}
		if !deadline.After(time.Now()) {
			return nil
		}
		<-ticker.C
	}
}
