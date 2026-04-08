package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// MoveQueuedJobResult summarizes a queued-job move operation.
type MoveQueuedJobResult struct {
	TargetDesc string
	InstanceID int64
}

// MoveQueuedJobToNewInstance launches a compatible new rental instance for a
// queued job and submits the job to it.
func MoveQueuedJobToNewInstance(database *sql.DB, jobID int64) (MoveQueuedJobResult, error) {
	if database == nil {
		return MoveQueuedJobResult{}, fmt.Errorf("database is required")
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return MoveQueuedJobResult{}, fmt.Errorf("get job %d: %w", jobID, err)
	}
	if job == nil {
		return MoveQueuedJobResult{}, fmt.Errorf("job %d not found", jobID)
	}
	if job.EffectiveStatus() != db.StatusQueued {
		return MoveQueuedJobResult{}, fmt.Errorf("can only move queued jobs (job %d has status: %s)", jobID, job.EffectiveStatus())
	}

	cfg, err := config.Load()
	if err != nil {
		return MoveQueuedJobResult{}, fmt.Errorf("load config: %w", err)
	}
	cloudClients, err := buildCloudClients(cfg)
	if err != nil {
		return MoveQueuedJobResult{}, fmt.Errorf("build cloud clients: %w", err)
	}
	r2Client, err := buildR2Client(cfg)
	if err != nil {
		return MoveQueuedJobResult{}, fmt.Errorf("build R2 client: %w", err)
	}

	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return MoveQueuedJobResult{}, fmt.Errorf("list running launches: %w", err)
	}
	capacities := make([]campaign.InstanceCapacity, 0, len(launches))
	queuedCounts := make(map[int64]int, len(launches))
	for _, ci := range launches {
		liveJobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if jobsErr != nil {
			continue
		}
		if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobs(liveJobs)); ok {
			capacities = append(capacities, cap)
		}
		for _, j := range liveJobs {
			if j != nil && j.EffectiveStatus() == db.StatusQueued {
				queuedCounts[ci.ID]++
			}
		}
	}

	sourceInstanceID := int64(0)
	if job.LaunchID != nil {
		sourceInstanceID = *job.LaunchID
	}
	optMsg := requestMoveOptions(database, cfg, cloudClients, job, capacities, queuedCounts, sourceInstanceID)()
	moveOptsMsg, ok := optMsg.(moveOptionsReadyMsg)
	if !ok {
		return MoveQueuedJobResult{}, fmt.Errorf("failed to compute move destinations")
	}
	if moveOptsMsg.err != nil {
		return MoveQueuedJobResult{}, moveOptsMsg.err
	}

	var selected *moveOption
	for i := range moveOptsMsg.options {
		if moveOptsMsg.options[i].isNew {
			selected = &moveOptsMsg.options[i]
			break
		}
	}
	if selected == nil {
		return MoveQueuedJobResult{}, fmt.Errorf("no compatible new-instance destination found")
	}

	execMsg := requestMoveExecute(context.Background(), database, r2Client, cfg, cloudClients, job.ID, *selected)()
	done, ok := execMsg.(moveExecuteDoneMsg)
	if !ok {
		return MoveQueuedJobResult{}, fmt.Errorf("move execution did not return a result")
	}
	if done.err != nil {
		return MoveQueuedJobResult{}, done.err
	}

	result := MoveQueuedJobResult{TargetDesc: strings.TrimSpace(done.targetDesc)}
	if done.targetDesc != "" {
		for _, token := range strings.Fields(done.targetDesc) {
			clean := strings.Trim(token, ",.;:()[]")
			id, parseErr := ids.ParseInstanceID(clean)
			if parseErr == nil && id > 0 {
				result.InstanceID = id
				break
			}
		}
	}
	return result, nil
}
