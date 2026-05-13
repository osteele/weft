package campaign

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
)

func recordCampaignPlacementTelemetry(database *sql.DB, cfg *config.Config, campaignID, launchID int64, group InstanceGroup, offer cloud.Offer, estimate CostEstimate, profile bidding.ScoreProfile, alternativesByGroup map[string][]RankedOfferAlternative) {
	if database == nil {
		return
	}
	sampled := db.ShouldSample(fmt.Sprintf("campaign:%d:launch:%d", campaignID, launchID), cfg.PlacementAlternativeSampleRate())
	candidates := campaignPlacementCandidates(group, offer, cfg.PlacementAlternativeTopK(), alternativesByGroup[LaunchGroupSignature(group)])
	for _, job := range group.Jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		attemptID, err := db.GetLatestAttemptID(database, job.ID)
		if err != nil {
			slog.Warn("failed to load campaign attempt for placement telemetry", "job_id", job.ID, "error", err)
		}
		var attemptPtr *int64
		if attemptID > 0 {
			attemptPtr = &attemptID
		}
		selectedScore := selectedCampaignAlternativeScore(candidates, offer)
		_, err = db.RecordPlacementDecision(database, db.PlacementDecision{
			JobID:                  &job.ID,
			AttemptID:              attemptPtr,
			CampaignID:             &campaignID,
			DecisionKind:           "acted",
			Operation:              "campaign_launch",
			Objective:              profile.ID,
			PlacementPolicyVersion: placement.PolicyVersion,
			SelectedKind:           "cloud-offer",
			SelectedTarget:         offer.Key(),
			SelectedScore:          selectedScore,
			SampleCandidates:       sampled,
		}, candidates)
		if err != nil {
			slog.Warn("failed to record campaign placement telemetry", "job_id", job.ID, "campaign_id", campaignID, "error", err)
		}
		if duration, ok := estimate.JobDurations[job.ID]; ok && duration > 0 {
			metadata := estimate.JobRuntimeMetadata[job.ID]
			err = db.RecordPredictionHistory(database, db.PredictionHistory{
				JobID:            job.ID,
				AttemptID:        attemptPtr,
				Target:           "duration",
				Host:             offer.Key(),
				GPUClass:         offer.GPUName,
				ModelFingerprint: metadata.ModelFingerprint,
				Prediction: map[string]any{
					"duration_s": duration.Seconds(),
				},
				Metadata: metadata,
			})
			if err != nil {
				slog.Warn("failed to record campaign prediction history", "job_id", job.ID, "campaign_id", campaignID, "error", err)
			}
		}
	}
}

// PlacementAlternativesByLaunchGroup indexes ranked alternatives from a launch
// plan by stable group signature so LaunchCampaign can persist them after the
// instances and attempts exist.
func PlacementAlternativesByLaunchGroup(groupOffers []GroupOffer) map[string][]RankedOfferAlternative {
	if len(groupOffers) == 0 {
		return nil
	}
	out := make(map[string][]RankedOfferAlternative, len(groupOffers))
	for _, groupOffer := range groupOffers {
		if len(groupOffer.Alternatives) == 0 {
			continue
		}
		out[LaunchGroupSignature(groupOffer.Group)] = append([]RankedOfferAlternative(nil), groupOffer.Alternatives...)
	}
	return out
}

func campaignPlacementCandidates(group InstanceGroup, selected cloud.Offer, limit int, alternatives []RankedOfferAlternative) []db.PlacementCandidate {
	if limit <= 0 {
		return nil
	}
	if len(alternatives) == 0 {
		alternatives = append(alternatives, RankedOfferAlternative{
			Rank:     1,
			Offer:    selected,
			Selected: true,
		})
	}
	out := make([]db.PlacementCandidate, 0, minInt(limit, len(alternatives)))
	for _, alt := range alternatives {
		if len(out) >= limit {
			break
		}
		rank := alt.Rank
		if rank == 0 {
			rank = len(out) + 1
		}
		score := alt.Score
		estTime := alt.CompletionHrs * 3600
		cost := alt.Cost
		survival := alt.Survival
		out = append(out, db.PlacementCandidate{
			Rank:          rank,
			CandidateKind: "cloud-offer",
			Target:        alt.Offer.Key(),
			Provider:      string(alt.Offer.Provider),
			OfferID:       alt.Offer.ProviderID,
			GPUName:       alt.Offer.GPUName,
			Score:         &score,
			EstTimeS:      &estTime,
			EstCost:       &cost,
			Survival:      &survival,
			Selected:      alt.Selected || alt.Offer.Key() == selected.Key(),
			Details: map[string]any{
				"job_count":     len(group.Jobs),
				"cost_per_hour": alt.Offer.CostPerHour,
				"dl_perf":       alt.Offer.DLPerf,
				"data_center":   alt.Offer.DataCenter,
			},
		})
	}
	return out
}

func selectedCampaignAlternativeScore(candidates []db.PlacementCandidate, selected cloud.Offer) *float64 {
	for _, c := range candidates {
		if c.Target == selected.Key() && c.Score != nil {
			return c.Score
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
