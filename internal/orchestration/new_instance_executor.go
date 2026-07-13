package orchestration

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/retrypolicy"
)

type newInstanceLaunchFunc func(
	clients []cloud.Client,
	database *sql.DB,
	groups []campaign.InstanceGroup,
	offers []cloud.Offer,
	estimates []campaign.CostEstimate,
	survivalModel *bidding.SurvivalModel,
	opts campaign.LaunchOpts,
	r2Cfg cloud.R2Config,
	createOptsForProvider func(cloud.Provider) (cloud.CreateOpts, error),
	onEvent func(event campaign.LaunchEvent),
	onCampaignCreated func(id int64),
	onInstanceRegistered func(group campaign.InstanceGroup, instanceID int64),
) (*campaign.LaunchResult, error)

type newInstanceLaunchExecutionOptions struct {
	Database             *sql.DB
	Clients              []cloud.Client
	Groups               []campaign.InstanceGroup
	Offers               []cloud.Offer
	Estimates            []campaign.CostEstimate
	SurvivalModel        *bidding.SurvivalModel
	LaunchOpts           campaign.LaunchOpts
	R2Config             cloud.R2Config
	CreateOptions        func(cloud.Provider) (cloud.CreateOpts, error)
	OnEvent              func(campaign.LaunchEvent)
	OnCampaignCreated    func(int64)
	OnInstanceRegistered func(campaign.InstanceGroup, int64)
	LaunchCampaign       newInstanceLaunchFunc
}

type newInstanceLaunchExecution struct {
	Result    *campaign.LaunchResult
	IntentIDs map[int64]int64
	database  *sql.DB
}

func defaultMoveToNewMaxAttempts() int {
	return retrypolicy.MaxPlacementAttempts()
}

func executeNewInstanceLaunchWithMoveIntents(opts newInstanceLaunchExecutionOptions) (*newInstanceLaunchExecution, error) {
	if opts.Database == nil {
		return nil, fmt.Errorf("database is required")
	}
	intentIDs, err := openMoveIntentsForNewInstanceGroups(opts.Database, opts.Groups, opts.Offers)
	if err != nil {
		return nil, err
	}
	launch := opts.LaunchCampaign
	if launch == nil {
		launch = campaign.LaunchCampaign
	}
	launchOpts := opts.LaunchOpts
	launchOpts.MoveTargetClaim = true
	result, err := launch(
		opts.Clients,
		opts.Database,
		opts.Groups,
		opts.Offers,
		opts.Estimates,
		opts.SurvivalModel,
		launchOpts,
		opts.R2Config,
		opts.CreateOptions,
		opts.OnEvent,
		opts.OnCampaignCreated,
		func(group campaign.InstanceGroup, instanceID int64) {
			updateMoveIntentTargetsForGroup(opts.Database, intentIDs, group, instanceID)
			if opts.OnInstanceRegistered != nil {
				opts.OnInstanceRegistered(group, instanceID)
			}
		},
	)
	execution := &newInstanceLaunchExecution{
		Result:    result,
		IntentIDs: intentIDs,
		database:  opts.Database,
	}
	if err != nil {
		execution.Cancel("new instance launch failed")
		return execution, err
	}
	return execution, nil
}

func (e *newInstanceLaunchExecution) Confirm(resolution string) {
	if e == nil {
		return
	}
	resolveMoveIntentIDs(e.database, e.IntentIDs, db.MoveIntentStateConfirmed, resolution)
}

func (e *newInstanceLaunchExecution) Cancel(resolution string) {
	if e == nil {
		return
	}
	resolveMoveIntentIDs(e.database, e.IntentIDs, db.MoveIntentStateCanceled, resolution)
}

func openMoveIntentsForNewInstanceGroups(database *sql.DB, groups []campaign.InstanceGroup, offers []cloud.Offer) (map[int64]int64, error) {
	intentIDs := make(map[int64]int64)
	for groupIndex, group := range groups {
		offer := cloud.Offer{}
		if groupIndex < len(offers) {
			offer = offers[groupIndex]
		}
		for _, job := range group.Jobs {
			if job == nil || job.ID <= 0 {
				continue
			}
			if err := supersedeOpenPlacementForMove(database, job.ID); err != nil {
				resolveMoveIntentIDs(database, intentIDs, db.MoveIntentStateCanceled, "new instance planning failed")
				return nil, fmt.Errorf("replace prior move for job %s: %w", ids.FormatJobID(job.ID), err)
			}
			intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
				JobID:               job.ID,
				TargetKind:          db.MoveTargetNew,
				TargetOfferProvider: string(offer.Provider),
				TargetOfferID:       offer.ProviderID,
				TargetGPUName:       offer.GPUName,
				AttemptCount:        1,
				MaxAttempts:         defaultMoveToNewMaxAttempts(),
			})
			if err != nil {
				resolveMoveIntentIDs(database, intentIDs, db.MoveIntentStateCanceled, "new instance planning failed")
				return nil, fmt.Errorf("open move intent for job %s: %w", ids.FormatJobID(job.ID), err)
			}
			intentIDs[job.ID] = intent.ID
		}
	}
	return intentIDs, nil
}

func orderedMoveIntentIDs(groups []campaign.InstanceGroup, intentIDs map[int64]int64) []int64 {
	out := make([]int64, 0, len(intentIDs))
	for _, group := range groups {
		for _, job := range group.Jobs {
			if job == nil {
				continue
			}
			if id := intentIDs[job.ID]; id > 0 {
				out = append(out, id)
			}
		}
	}
	return out
}

func updateMoveIntentTargetsForGroup(database *sql.DB, intentIDs map[int64]int64, group campaign.InstanceGroup, instanceID int64) {
	for _, job := range group.Jobs {
		if job == nil {
			continue
		}
		intentID := intentIDs[job.ID]
		if intentID <= 0 {
			continue
		}
		if err := db.UpdateMoveIntentTargetLaunch(database, intentID, instanceID); err != nil {
			slog.Warn("update move target launch", "component", "move", "job_id", job.ID, "launch_id", instanceID, "error", err)
		}
	}
}

func resolveMoveIntentIDs(database *sql.DB, intentIDs map[int64]int64, state db.MoveIntentState, resolution string) {
	if database == nil {
		return
	}
	for _, intentID := range intentIDs {
		if intentID <= 0 {
			continue
		}
		if err := db.ResolveMoveIntent(database, intentID, state, resolution); err != nil {
			slog.Warn("resolve move intent", "component", "move", "intent_id", intentID, "state", state, "error", err)
		}
	}
}
