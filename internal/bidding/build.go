package bidding

import (
	"database/sql"
	"sort"
)

// InstanceOutcome holds the data needed to build the survival model from one instance.
type InstanceOutcome struct {
	TerminationReason string
	CostPerHourCents  int
	ResolvedGPUName   string
	Reliability       float64
}

// LoadInstanceOutcomes queries terminal cloud instances that have termination reasons.
func LoadInstanceOutcomes(db *sql.DB) ([]InstanceOutcome, error) {
	rows, err := db.Query(`
		SELECT termination_reason, cost_per_hour_cents, resolved_gpu_name, reliability
		FROM cloud_instances
		WHERE status IN ('completed', 'failed', 'cancelled')
		  AND termination_reason IS NOT NULL
		  AND termination_reason != ''
		  AND launched_at IS NOT NULL
		  AND resolved_gpu_name IS NOT NULL
		  AND resolved_gpu_name != ''
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var outcomes []InstanceOutcome
	for rows.Next() {
		var o InstanceOutcome
		var costCents sql.NullInt64
		var reliability sql.NullFloat64
		if err := rows.Scan(&o.TerminationReason, &costCents, &o.ResolvedGPUName, &reliability); err != nil {
			return nil, err
		}
		if costCents.Valid {
			o.CostPerHourCents = int(costCents.Int64)
		}
		if reliability.Valid {
			o.Reliability = reliability.Float64
		}
		outcomes = append(outcomes, o)
	}
	return outcomes, rows.Err()
}

// BuildSurvivalModel constructs a SurvivalModel from instance outcomes.
// Returns nil if there are no outcomes.
func BuildSurvivalModel(outcomes []InstanceOutcome) *SurvivalModel {
	if len(outcomes) == 0 {
		return nil
	}

	// Collect prices per GPU family for percentile computation
	familyPrices := make(map[string][]float64)
	for _, o := range outcomes {
		fam := NormalizeGPUFamily(o.ResolvedGPUName)
		price := float64(o.CostPerHourCents) / 100.0
		familyPrices[fam] = append(familyPrices[fam], price)
	}

	// Sort prices for percentile lookup
	for fam := range familyPrices {
		sort.Float64s(familyPrices[fam])
	}

	model := &SurvivalModel{
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: familyPrices,
		PriorStrength:    defaultPriorStrength,
	}

	// Bucket instances and accumulate stats
	for _, o := range outcomes {
		survived := isSurvived(o.TerminationReason)

		model.GlobalTotal++
		if survived {
			model.GlobalSurvived++
		}

		fam := NormalizeGPUFamily(o.ResolvedGPUName)
		price := float64(o.CostPerHourCents) / 100.0
		bucket := model.PriceBucketFor(fam, price)
		key := groupKey(fam, bucket)

		gs, ok := model.Groups[key]
		if !ok {
			gs = &SurvivalStats{}
			model.Groups[key] = gs
		}
		gs.Total++
		if survived {
			gs.Survived++
		}
	}

	return model
}

const defaultPriorStrength = 10.0

// isSurvived returns true if the termination reason indicates the instance
// completed its work (was not preempted or lost to infra failure).
func isSurvived(reason string) bool {
	return reason == "completed" || reason == "job_failure"
}
