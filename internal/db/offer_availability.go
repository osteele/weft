package db

import (
	"database/sql"
	"fmt"
	"time"
)

const offerAvailabilitySnapshotMinInterval = 15 * time.Minute

// OfferAvailabilitySnapshot is a compact market-observation row for one
// provider/search bucket. It records what the provider returned now; planner
// behavior must not depend on whether this telemetry write succeeds.
type OfferAvailabilitySnapshot struct {
	Provider          string
	GPUClass          string
	GPUMemBucketGB    int
	DiskBucketGB      int
	NumGPUs           int
	Interconnect      string
	InstanceType      string
	RunpodCloudType   string
	MinReliability    float64
	OfferCount        int
	PostFilterCount   *int
	PriceMinCents     *int
	PriceMedianCents  *int
	PriceP75Cents     *int
	Details           any
	RateLimitInterval time.Duration
}

// RecordOfferAvailabilitySnapshot persists one rate-limited offer-market
// observation. Repeated observations for the same bucket within the interval
// are intentionally skipped to keep autopilot passes from bloating the DB.
func RecordOfferAvailabilitySnapshot(database *sql.DB, snapshot OfferAvailabilitySnapshot) error {
	if database == nil {
		return nil
	}
	if snapshot.NumGPUs <= 0 {
		snapshot.NumGPUs = 1
	}
	interval := snapshot.RateLimitInterval
	if interval <= 0 {
		interval = offerAvailabilitySnapshotMinInterval
	}
	now := time.Now().Unix()
	cutoff := now - int64(interval.Seconds())
	details, err := jsonOrNull(snapshot.Details)
	if err != nil {
		return fmt.Errorf("encode offer availability details: %w", err)
	}
	_, err = database.Exec(`
		INSERT INTO offer_availability_snapshots (
			created_at, provider, gpu_class, gpu_mem_bucket_gb, disk_bucket_gb,
			num_gpus, interconnect, instance_type, runpod_cloud_type,
			min_reliability, offer_count, post_filter_count, price_min_cents,
			price_median_cents, price_p75_cents, details_json
		)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM offer_availability_snapshots
			WHERE provider = ?
			  AND gpu_class = ?
			  AND gpu_mem_bucket_gb = ?
			  AND disk_bucket_gb = ?
			  AND num_gpus = ?
			  AND interconnect = ?
			  AND instance_type = ?
			  AND runpod_cloud_type = ?
			  AND min_reliability = ?
			  AND created_at >= ?
		)`,
		now,
		snapshot.Provider,
		snapshot.GPUClass,
		snapshot.GPUMemBucketGB,
		snapshot.DiskBucketGB,
		snapshot.NumGPUs,
		snapshot.Interconnect,
		snapshot.InstanceType,
		snapshot.RunpodCloudType,
		snapshot.MinReliability,
		snapshot.OfferCount,
		nullableIntValue(snapshot.PostFilterCount),
		nullableIntValue(snapshot.PriceMinCents),
		nullableIntValue(snapshot.PriceMedianCents),
		nullableIntValue(snapshot.PriceP75Cents),
		details,
		snapshot.Provider,
		snapshot.GPUClass,
		snapshot.GPUMemBucketGB,
		snapshot.DiskBucketGB,
		snapshot.NumGPUs,
		snapshot.Interconnect,
		snapshot.InstanceType,
		snapshot.RunpodCloudType,
		snapshot.MinReliability,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("record offer availability snapshot: %w", err)
	}
	return nil
}

func nullableIntValue(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
