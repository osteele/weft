package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"
)

// PlacementDecision is an acted or shadow scheduling decision.
type PlacementDecision struct {
	JobID                  *int64
	AttemptID              *int64
	CampaignID             *int64
	DecisionKind           string
	Operation              string
	Objective              string
	PlacementPolicyVersion string
	ModelFingerprint       string
	SelectedKind           string
	SelectedTarget         string
	SelectedScore          *float64
	SampleCandidates       bool
}

// PlacementCandidate is a ranked placement alternative attached to a decision.
type PlacementCandidate struct {
	Rank          int
	CandidateKind string
	Target        string
	Provider      string
	OfferID       string
	GPUName       string
	Score         *float64
	EstTimeS      *float64
	EstCost       *float64
	Survival      *float64
	Selected      bool
	Reasons       []string
	Details       any
}

// PredictionHistory stores the prediction payload used for one target.
type PredictionHistory struct {
	JobID            int64
	AttemptID        *int64
	Target           string
	Host             string
	GPUClass         string
	Prediction       any
	Level0           any
	Level1           any
	Level2           any
	Level3           any
	ModelFingerprint string
	Metadata         any
}

// RecordPlacementDecision inserts a decision and, when sampled, its candidates.
func RecordPlacementDecision(database *sql.DB, decision PlacementDecision, candidates []PlacementCandidate) (int64, error) {
	if database == nil {
		return 0, nil
	}
	now := time.Now().Unix()
	var selectedScore any
	if decision.SelectedScore != nil {
		selectedScore = *decision.SelectedScore
	}
	res, err := database.Exec(`
		INSERT INTO placement_decisions (
			job_id, attempt_id, campaign_id, decision_kind, operation, objective,
			placement_policy_version, model_fingerprint, selected_kind,
			selected_target, selected_score, sample_candidates, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullableInt64(decision.JobID),
		nullableInt64(decision.AttemptID),
		nullableInt64(decision.CampaignID),
		decision.DecisionKind,
		decision.Operation,
		decision.Objective,
		decision.PlacementPolicyVersion,
		decision.ModelFingerprint,
		decision.SelectedKind,
		decision.SelectedTarget,
		selectedScore,
		boolInt(decision.SampleCandidates),
		now,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if !decision.SampleCandidates {
		return id, nil
	}
	for _, c := range candidates {
		if err := insertPlacementCandidate(database, id, c); err != nil {
			return id, err
		}
	}
	return id, nil
}

func insertPlacementCandidate(database *sql.DB, decisionID int64, c PlacementCandidate) error {
	reasons, err := jsonOrNull(c.Reasons)
	if err != nil {
		return err
	}
	details, err := jsonOrNull(c.Details)
	if err != nil {
		return err
	}
	_, err = database.Exec(`
		INSERT INTO placement_candidates (
			decision_id, rank, candidate_kind, target, provider, offer_id,
			gpu_name, score, est_time_s, est_cost, survival, selected,
			reasons_json, details_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		decisionID,
		c.Rank,
		c.CandidateKind,
		c.Target,
		c.Provider,
		c.OfferID,
		c.GPUName,
		nullableFloat64(c.Score),
		nullableFloat64(c.EstTimeS),
		nullableFloat64(c.EstCost),
		nullableFloat64(c.Survival),
		boolInt(c.Selected),
		reasons,
		details,
	)
	return err
}

// RecordPredictionHistory inserts durable prediction payloads for later MAPE
// and model-comparison analysis.
func RecordPredictionHistory(database *sql.DB, h PredictionHistory) error {
	if database == nil || h.JobID <= 0 || h.Target == "" {
		return nil
	}
	prediction, err := jsonOrNull(h.Prediction)
	if err != nil {
		return err
	}
	level0, err := jsonOrNull(h.Level0)
	if err != nil {
		return err
	}
	level1, err := jsonOrNull(h.Level1)
	if err != nil {
		return err
	}
	level2, err := jsonOrNull(h.Level2)
	if err != nil {
		return err
	}
	level3, err := jsonOrNull(h.Level3)
	if err != nil {
		return err
	}
	metadata, err := jsonOrNull(h.Metadata)
	if err != nil {
		return err
	}
	_, err = database.Exec(`
		INSERT INTO prediction_history (
			job_id, attempt_id, target, host, gpu_class, prediction_json,
			level0_json, level1_json, level2_json, level3_json,
			model_fingerprint, metadata_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.JobID,
		nullableInt64(h.AttemptID),
		h.Target,
		h.Host,
		h.GPUClass,
		prediction,
		level0,
		level1,
		level2,
		level3,
		h.ModelFingerprint,
		metadata,
		time.Now().Unix(),
	)
	return err
}

// RecordDonorExperiment stores the donor-vs-hub campaign assignment.
func RecordDonorExperiment(database *sql.DB, campaignID int64, cohort string, sampleRate float64, donorLaunchID *int64, reason string) error {
	if database == nil || campaignID <= 0 || cohort == "" {
		return nil
	}
	_, err := database.Exec(`
		INSERT OR REPLACE INTO donor_experiments (
			campaign_id, cohort, sample_rate, donor_launch_id, reason, assigned_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		campaignID,
		cohort,
		sampleRate,
		nullableInt64(donorLaunchID),
		reason,
		time.Now().Unix(),
	)
	return err
}

// ShouldSample returns a deterministic Bernoulli decision for a stable key.
func ShouldSample(key string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	bucket := float64(h.Sum64()%1_000_000) / 1_000_000
	return bucket < rate
}

func nullableFloat64(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func jsonOrNull(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode scheduling telemetry json: %w", err)
	}
	return string(data), nil
}
