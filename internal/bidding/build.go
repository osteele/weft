package bidding

import (
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	jobdb "github.com/osteele/weft/internal/db"
)

// InstanceOutcome holds the data needed to build the survival model from one instance.
type InstanceOutcome struct {
	Provider          cloud.Provider
	TerminationReason string
	TerminationDetail string
	SurvivalClass     SurvivalTrainingClass
	CostPerHourCents  int
	ResolvedGPUName   string
	GPUMemGB          int // per-GPU memory; partitions same-family SKUs (e.g. 4090 24GB vs 48GB)
	DiskGB            int // requested container disk; used to identify legacy impossible-request bugs
	Reliability       float64
	MachineID         string
	DataCenter        string // provider-reported region+country, e.g. "Sichuan, CN" or ", US" (region missing)
	EndedAtUnix       int64  // 0 if unknown; used by the recency-decay weighting
}

// SurvivalTrainingClass is the structured interpretation of a launch outcome
// for the provider/SKU survival model. It deliberately sits below
// launches.termination_reason: old rows keep their raw reason/detail evidence,
// while this classifier can evolve as we learn which legacy failures were
// provider-attributable and which were Weft/setup/staging bugs.
type SurvivalTrainingClass string

const (
	SurvivalTrainSurvived                SurvivalTrainingClass = "survived"
	SurvivalTrainProviderFailure         SurvivalTrainingClass = "provider_failure"
	SurvivalTrainExcludedUserCancelled   SurvivalTrainingClass = "excluded_user_cancelled"
	SurvivalTrainExcludedAccountCredit   SurvivalTrainingClass = "excluded_account_credit"
	SurvivalTrainExcludedWeftBug         SurvivalTrainingClass = "excluded_weft_bug"
	SurvivalTrainExcludedProviderAPI     SurvivalTrainingClass = "excluded_provider_api"
	SurvivalTrainExcludedLocalStaging    SurvivalTrainingClass = "excluded_local_staging"
	SurvivalTrainExcludedSetup           SurvivalTrainingClass = "excluded_setup"
	SurvivalTrainExcludedInvalidRequest  SurvivalTrainingClass = "excluded_invalid_request"
	SurvivalTrainExcludedMissingEvidence SurvivalTrainingClass = "excluded_missing_evidence"
)

func (c SurvivalTrainingClass) Trainable() bool {
	return c == SurvivalTrainSurvived || c == SurvivalTrainProviderFailure
}

func (c SurvivalTrainingClass) Survived() bool {
	return c == SurvivalTrainSurvived
}

const impossibleRequestDiskGB = 2000

// ClassifySurvivalTrainingOutcome maps a raw launch terminal reason/detail to
// the structured training class used by the bidding survival model.
func ClassifySurvivalTrainingOutcome(reason, detail string, diskGB int) SurvivalTrainingClass {
	reason = strings.TrimSpace(reason)
	detailLower := strings.ToLower(detail)

	if isSurvived(reason) {
		return SurvivalTrainSurvived
	}
	if jobdb.ClassifyCreditSignal(detail) == jobdb.CreditSignalStrong {
		return SurvivalTrainExcludedAccountCredit
	}

	switch reason {
	case "":
		return SurvivalTrainExcludedMissingEvidence
	case jobdb.TerminationReasonCancelled:
		return SurvivalTrainExcludedUserCancelled
	case jobdb.TerminationReasonAccountCreditExhausted:
		return SurvivalTrainExcludedAccountCredit
	case jobdb.TerminationReasonWeftBug:
		return SurvivalTrainExcludedWeftBug
	case jobdb.TerminationReasonProviderTimeout:
		return SurvivalTrainExcludedProviderAPI
	case jobdb.TerminationReasonPhaseStall:
		return SurvivalTrainExcludedSetup
	}

	if strings.Contains(detailLower, "unknown flag: --min-cuda-version") {
		return SurvivalTrainExcludedWeftBug
	}
	if strings.Contains(detailLower, "manifest upload failed") ||
		strings.Contains(detailLower, "bootstrap upload failed") ||
		strings.Contains(detailLower, "r2 source upload failed") {
		return SurvivalTrainExcludedLocalStaging
	}
	if diskGB > impossibleRequestDiskGB && strings.Contains(detailLower, "disk") {
		return SurvivalTrainExcludedInvalidRequest
	}
	if strings.Contains(detailLower, "artifact needs staging failed") ||
		strings.Contains(detailLower, "cloud artifact staging failed") {
		return SurvivalTrainExcludedLocalStaging
	}

	return SurvivalTrainProviderFailure
}

// LoadInstanceOutcomes queries terminal cloud instances that have termination reasons.
// Rows missing a provider are skipped because survival statistics must be
// scoped per-provider (RunPod and Vast have different reliability baselines).
//
// Pre-creation failures are included when they are provider-offer signal.
// Account/client/setup/staging failures are classified and excluded because
// they would otherwise poison provider/SKU/region priors without saying
// anything about machine survival.
// Rows without machine attribution still contribute to provider/SKU buckets,
// while geoAdjustment naturally skips their per-machine cache.
func LoadInstanceOutcomes(db *sql.DB) ([]InstanceOutcome, error) {
	rows, err := db.Query(`
		SELECT provider, termination_reason, COALESCE(termination_detail, ''),
		       cost_per_hour_cents, resolved_gpu_name, reliability,
		       COALESCE(machine_id, ''), COALESCE(ended_at, 0), COALESCE(gpu_mem_gb, 0),
		       COALESCE(data_center, ''), COALESCE(disk_gb, 0)
		FROM launches
		WHERE status IN ('completed', 'failed', 'canceled')
		  AND termination_reason IS NOT NULL
		  AND termination_reason != ''
		  AND provider IS NOT NULL
		  AND provider != ''
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
		var provider string
		var costCents sql.NullInt64
		var reliability sql.NullFloat64
		if err := rows.Scan(&provider, &o.TerminationReason, &o.TerminationDetail, &costCents, &o.ResolvedGPUName, &reliability, &o.MachineID, &o.EndedAtUnix, &o.GPUMemGB, &o.DataCenter, &o.DiskGB); err != nil {
			return nil, err
		}
		o.SurvivalClass = ClassifySurvivalTrainingOutcome(o.TerminationReason, o.TerminationDetail, o.DiskGB)
		if !o.SurvivalClass.Trainable() {
			continue
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
		survivalClass := o.SurvivalClass
		if survivalClass == "" {
			survivalClass = ClassifySurvivalTrainingOutcome(o.TerminationReason, o.TerminationDetail, o.DiskGB)
		}
		if !survivalClass.Trainable() {
			continue
		}
		survived := survivalClass.Survived()
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
