package bidding

import (
	"database/sql"
	"math"
	"sort"
	"time"

	"github.com/osteele/weft/internal/cloud"
	jobdb "github.com/osteele/weft/internal/db"
)

// InstanceOutcome holds the data needed to build the survival model from one instance.
type InstanceOutcome struct {
	Provider          cloud.Provider
	TerminationReason string
	CostPerHourCents  int
	ResolvedGPUName   string
	GPUMemGB          int // per-GPU memory; partitions same-family SKUs (e.g. 4090 24GB vs 48GB)
	Reliability       float64
	MachineID         string
	DataCenter        string // provider-reported region+country, e.g. "Sichuan, CN" or ", US" (region missing)
	EndedAtUnix       int64  // 0 if unknown; used by the recency-decay weighting
}

// LoadInstanceOutcomes queries terminal cloud instances that have termination reasons.
// Rows missing a provider are skipped because survival statistics must be
// scoped per-provider (RunPod and Vast have different reliability baselines).
//
// Pre-creation failures are included when they are provider-offer signal.
// Account/client failures are excluded because they would otherwise poison
// provider/SKU/region priors without saying anything about machine survival.
// Rows without machine attribution still contribute to provider/SKU buckets,
// while geoAdjustment naturally skips their per-machine cache.
//
// Drift warning: the credit-exhaustion termination_detail patterns below
// duplicate the substring list in internal/vastai/client.go
// isAccountCreditError. The two must stay in sync until the proper fix
// lands — see docs/planning/ROADMAP.md § "Structured termination reasons
// for credit exhaustion" — which replaces both with a typed
// TerminationReason.
func LoadInstanceOutcomes(db *sql.DB) ([]InstanceOutcome, error) {
	rows, err := db.Query(`
		SELECT provider, termination_reason, cost_per_hour_cents, resolved_gpu_name, reliability,
		       COALESCE(machine_id, ''), COALESCE(ended_at, 0), COALESCE(gpu_mem_gb, 0),
		       COALESCE(data_center, '')
		FROM launches
		WHERE status IN ('completed', 'failed', 'canceled')
		  AND termination_reason IS NOT NULL
		  AND termination_reason != ''
		  AND termination_reason NOT IN (?, ?)
		  AND provider IS NOT NULL
		  AND provider != ''
		  AND resolved_gpu_name IS NOT NULL
		  AND resolved_gpu_name != ''
		  -- Credit-exhaustion phrase list — keep in sync with
		  -- internal/vastai/client.go isAccountCreditError.
		  AND lower(COALESCE(termination_detail, '')) NOT LIKE '%provider returned empty response%'
		  AND lower(COALESCE(termination_detail, '')) NOT LIKE '%insufficient balance%'
		  AND lower(COALESCE(termination_detail, '')) NOT LIKE '%account lacks credit%'
		  AND lower(COALESCE(termination_detail, '')) NOT LIKE '%account credit%'
		ORDER BY id
	`, jobdb.TerminationReasonCancelled, jobdb.TerminationReasonWeftBug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var outcomes []InstanceOutcome
	for rows.Next() {
		var o InstanceOutcome
		var provider string
		var costCents sql.NullInt64
		var reliability sql.NullFloat64
		if err := rows.Scan(&provider, &o.TerminationReason, &costCents, &o.ResolvedGPUName, &reliability, &o.MachineID, &o.EndedAtUnix, &o.GPUMemGB, &o.DataCenter); err != nil {
			return nil, err
		}
		o.Provider = cloud.Provider(provider)
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

// SurvivalDecayHalfLife controls how quickly old outcomes lose influence on
// the posterior. An outcome ended `SurvivalDecayHalfLife` ago contributes
// half as much weight as a fresh one; an outcome ended 2× this long ago
// contributes a quarter; etc. The raw counts (Total/Survived) are unaffected
// — they remain available for "minimum observations" thresholds and human
// display — but the Beta-Binomial posterior is computed from the weighted
// counts.
//
// Choosing 21 days: long enough that a steady stream of weekly campaigns
// keeps every active (provider, family) cell well-anchored, short enough
// that a stale exclusion (e.g. a provider outage from a month ago) decays
// out within a few cycles.
const SurvivalDecayHalfLife = 21 * 24 * time.Hour

// outcomeWeight returns the time-decay weight for an outcome ended at
// endedAt, evaluated at `now`. Outcomes with no ended_at recorded
// (endedAt==0) get full weight 1.0 — undated outcomes are typically test
// fixtures or pre-migration rows we want to keep counting.
func outcomeWeight(endedAt int64, now time.Time) float64 {
	if endedAt <= 0 {
		return 1.0
	}
	age := now.Sub(time.Unix(endedAt, 0))
	if age <= 0 {
		return 1.0
	}
	halfLives := float64(age) / float64(SurvivalDecayHalfLife)
	return math.Exp2(-halfLives)
}

// BuildSurvivalModel constructs a SurvivalModel from instance outcomes.
// Returns nil if there are no outcomes. All keys are scoped per-provider so
// outcomes from one provider do not influence posteriors for another.
//
// Outcomes are accumulated with both raw counts (for thresholds and
// display) and time-decayed weights (for posterior math). The decay clock
// runs against time.Now(); use BuildSurvivalModelAt for deterministic
// tests.
func BuildSurvivalModel(outcomes []InstanceOutcome) *SurvivalModel {
	return BuildSurvivalModelAt(outcomes, time.Now())
}

// BuildSurvivalModelAt is BuildSurvivalModel with the decay clock pinned to
// `now` for deterministic tests.
func BuildSurvivalModelAt(outcomes []InstanceOutcome, now time.Time) *SurvivalModel {
	if len(outcomes) == 0 {
		return nil
	}

	skuPrices := make(map[string][]float64)
	for _, o := range outcomes {
		sku := skuFromOutcome(NormalizeGPUFamily(o.ResolvedGPUName), o.GPUMemGB)
		price := float64(o.CostPerHourCents) / 100.0
		k := pricePercentileKey(o.Provider, sku)
		skuPrices[k] = append(skuPrices[k], price)
	}

	for k := range skuPrices {
		sort.Float64s(skuPrices[k])
	}

	model := &SurvivalModel{
		Global:           make(map[cloud.Provider]*SurvivalStats),
		Groups:           make(map[string]*SurvivalStats),
		MachineStats:     make(map[string]*SurvivalStats),
		CountryStats:     make(map[string]*SurvivalStats),
		RegionStats:      make(map[string]*SurvivalStats),
		PricePercentiles: skuPrices,
		PriorStrength:    defaultPriorStrength,
	}

	healthCutoff := now.Add(-HealthWindow).Unix()

	for _, o := range outcomes {
		survived := isSurvived(o.TerminationReason)
		weight := outcomeWeight(o.EndedAtUnix, now)

		// Recent counts use unweighted samples — the Beta priors handle
		// long-history shrinkage; this signal exists specifically to
		// catch what those priors are too slow to see.
		if o.EndedAtUnix > 0 && o.EndedAtUnix >= healthCutoff {
			model.RecentTotal++
			if survived {
				model.RecentSurvived++
			}
		}

		gs, ok := model.Global[o.Provider]
		if !ok {
			gs = &SurvivalStats{}
			model.Global[o.Provider] = gs
		}
		addOutcome(gs, survived, weight)

		sku := skuFromOutcome(NormalizeGPUFamily(o.ResolvedGPUName), o.GPUMemGB)
		price := float64(o.CostPerHourCents) / 100.0
		bucket := model.PriceBucketFor(o.Provider, sku, price)
		accumulateStats(model.Groups, groupKey(o.Provider, sku, bucket), survived, weight)

		if o.MachineID != "" {
			accumulateStats(model.MachineStats, machineKey(o.Provider, o.MachineID), survived, weight)
		}
		if o.DataCenter != "" {
			accumulateStats(model.RegionStats, regionKey(o.Provider, o.DataCenter), survived, weight)
		}
		if country := parseCountryFromDataCenter(o.DataCenter); country != "" {
			accumulateStats(model.CountryStats, countryKey(o.Provider, country), survived, weight)
		}
	}

	return model
}

const defaultPriorStrength = 3.0

func accumulateStats(m map[string]*SurvivalStats, key string, survived bool, weight float64) {
	s, ok := m[key]
	if !ok {
		s = &SurvivalStats{}
		m[key] = s
	}
	addOutcome(s, survived, weight)
}

func addOutcome(s *SurvivalStats, survived bool, weight float64) {
	s.Total++
	s.WeightedTotal += weight
	if survived {
		s.Survived++
		s.WeightedSurvived += weight
	}
}

// isSurvived returns true if the termination reason indicates the instance
// ran its workload (was not lost to provider or infrastructure failure).
// "exited" and "disk_full" count as survived because the job actually ran.
func isSurvived(reason string) bool {
	return reason == "completed" || reason == "job_failure" || reason == "disk_full" || reason == "exited"
}
