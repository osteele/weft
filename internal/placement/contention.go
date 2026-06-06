package placement

import (
	"database/sql"
	"time"
)

// DefaultContentionFactor assumes 50% runtime overhead when no live
// metrics or historical contention data are available.
const DefaultContentionFactor = 1.5

// RecordContentionObs saves a host contention snapshot to the database.
func RecordContentionObs(db *sql.DB, host string, metrics *HostMetrics) {
	if db == nil || metrics == nil {
		return
	}
	_, _ = db.Exec(`INSERT INTO host_contention_obs (host, gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		host, metrics.GPUPercent, metrics.CPUPercent, metrics.QueueDepth, metrics.GPUJobsQueued,
		time.Now().Unix())
}

// RecentHostMetrics returns latest contention observations for all requested
// hosts when every host has an observation newer than maxAge. Missing or stale
// observations make the snapshot unusable for synchronous no-probe placement.
func RecentHostMetrics(db *sql.DB, hosts []string, maxAge time.Duration) (map[string]*HostMetrics, bool, error) {
	if db == nil || len(hosts) == 0 {
		return nil, false, nil
	}
	cutoff := time.Now().Add(-maxAge).Unix()
	metrics := make(map[string]*HostMetrics, len(hosts))
	for _, host := range hosts {
		row := db.QueryRow(`SELECT gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at
			FROM host_contention_obs
			WHERE host = ?
			ORDER BY observed_at DESC
			LIMIT 1`, host)
		var m HostMetrics
		var observedAt int64
		if err := row.Scan(&m.GPUPercent, &m.CPUPercent, &m.QueueDepth, &m.GPUJobsQueued, &observedAt); err != nil {
			if err == sql.ErrNoRows {
				return nil, false, nil
			}
			return nil, false, err
		}
		if observedAt < cutoff {
			return nil, false, nil
		}
		metrics[host] = &m
	}
	return metrics, true, nil
}

// ContentionStats summarizes recent contention observations for a host.
type ContentionStats struct {
	AvgGPUPct        float64
	AvgQueueDepth    float64
	ObservationCount int
}

// RecentContentionStats returns averaged contention stats for a host over the last N hours.
func RecentContentionStats(db *sql.DB, host string, hours int) *ContentionStats {
	if db == nil {
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	row := db.QueryRow(`SELECT AVG(gpu_pct), AVG(queue_depth), COUNT(*)
		FROM host_contention_obs WHERE host = ? AND observed_at > ?`, host, cutoff)

	var stats ContentionStats
	var avgGPU, avgQueue sql.NullFloat64
	if err := row.Scan(&avgGPU, &avgQueue, &stats.ObservationCount); err != nil || stats.ObservationCount == 0 {
		return nil
	}
	if avgGPU.Valid {
		stats.AvgGPUPct = avgGPU.Float64
	}
	if avgQueue.Valid {
		stats.AvgQueueDepth = avgQueue.Float64
	}
	return &stats
}

// ContentionFactor returns a multiplier for job runtime based on host contention.
// Uses live metrics if available, otherwise falls back to historical stats,
// otherwise returns DefaultContentionFactor.
func ContentionFactor(db *sql.DB, host string, metrics *HostMetrics) float64 {
	if metrics != nil {
		// Live data available: scale linearly from 1.0 (idle) to 2.0 (fully loaded)
		return 1.0 + float64(metrics.GPUPercent)/100.0
	}
	if stats := RecentContentionStats(db, host, 24); stats != nil {
		return 1.0 + stats.AvgGPUPct/100.0
	}
	return DefaultContentionFactor
}
