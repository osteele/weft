package campaign

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/placement"
)

// ReuseSource implements placement.CandidateSource by finding reusable
// cloud instances (running or in grace period).
type ReuseSource struct{}

func (s *ReuseSource) Collect(database *sql.DB, constraints placement.Constraints, _ placement.JobPredictor) ([]placement.Candidate, error) {
	instances, err := FindReusableInstances(database)
	if err != nil || len(instances) == 0 {
		return nil, nil
	}

	job := &db.Job{
		GPUClass: constraints.GPUClass,
		Inputs:   constraints.Inputs,
	}
	if constraints.GPUMemGB > 0 {
		v := constraints.GPUMemGB
		job.GPUMemGB = &v
	}

	ranked := RankForJob(job, instances)
	if len(ranked) == 0 {
		return nil, nil
	}

	preferred := make(map[int64]bool, len(constraints.PreferredInstanceIDs))
	for _, id := range constraints.PreferredInstanceIDs {
		preferred[id] = true
	}

	var candidates []placement.Candidate
	for _, cap := range ranked {
		inst := cap.Instance
		if constraints.Provider != "" && !strings.EqualFold(inst.Provider, constraints.Provider) {
			continue
		}

		// Estimate queue wait on this instance. Preferred instances (e.g.
		// the consumer's --needs producer) get a zero-wait estimate so they
		// outrank equivalently-priced candidates; this is a soft tip, not a
		// hard requirement — filters above still apply.
		var waitMin float64
		if cap.RunningJobCount > 0 && !preferred[inst.ID] {
			waitMin = float64(cap.RunningJobCount) * 30
		}
		waitEst := estimate.FromSeconds(waitMin*60, waitMin*60*0.5, waitMin*60*1.5)

		runEst := estimate.FromSeconds(3600, 900, 14400)
		totalEst := waitEst.Add(runEst)

		costPerHour := float64(inst.CostPerHourCents) / 100.0
		status := string(inst.Status)

		opt := placement.ReuseOption{
			InstanceID:  inst.ID,
			DisplayName: fmt.Sprintf("#%d (%s)", inst.ID, inst.DisplayGPUSpec()),
			GPUClass:    inst.GPUClass,
			GPUMemGB:    inst.GPUMemGB,
			Status:      status,
			EstWait:     waitEst,
			CostPerHour: costPerHour,
		}

		var estCost float64
		if inst.Status != db.LaunchStatusGrace {
			estCost = costPerHour * totalEst.Mean.Hours()
		}

		candidates = append(candidates, placement.Candidate{
			Kind:        placement.CandidateCloudReuse,
			ID:          fmt.Sprintf("instance:%d", inst.ID),
			DisplayName: opt.DisplayName,
			EstTime:     totalEst,
			EstCost:     estCost,
			Survival:    placement.DefaultReuseSurvival,
			Reuse:       &opt,
		})
	}

	return candidates, nil
}
