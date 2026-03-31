package placement

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/estimate"
)

// RentalEstimate holds the estimated completion time and cost for a rental instance.
type RentalEstimate struct {
	LaunchEst   estimate.Estimate // instance startup + SSH + job setup
	TransferEst estimate.Estimate // model/data download
	RunEst      estimate.Estimate // job execution
	Total       estimate.Estimate // sum of all phases
	CostPerHour float64           // $/hr from historical launches
}

// rentalBaseline holds historical stats for a GPU class from recent launches.
type rentalBaseline struct {
	CostPerHourCents int
	DLPerf           float64
	InetDownMbps     float64
	Count            int
}

// recentRentalBaseline queries the launches table for the median cost/perf of
// recent completed launches with the given GPU class.
func recentRentalBaseline(db *sql.DB, gpuClass string) *rentalBaseline {
	if db == nil || gpuClass == "" {
		return nil
	}
	row := db.QueryRow(`
		SELECT AVG(cost_per_hour_cents), AVG(dl_perf), AVG(inet_down_mbps), COUNT(*)
		FROM launches
		WHERE gpu_class = ? AND status IN ('completed', 'grace', 'released')
		AND created_at > ?
		AND cost_per_hour_cents > 0`,
		gpuClass, time.Now().Add(-7*24*time.Hour).Unix())

	var b rentalBaseline
	var costCents, dlPerf, inetDown sql.NullFloat64
	if err := row.Scan(&costCents, &dlPerf, &inetDown, &b.Count); err != nil || b.Count == 0 {
		return nil
	}
	if costCents.Valid {
		b.CostPerHourCents = int(costCents.Float64)
	}
	if dlPerf.Valid {
		b.DLPerf = dlPerf.Float64
	}
	if inetDown.Valid {
		b.InetDownMbps = inetDown.Float64
	}
	return &b
}

// EstimateRentalCompletion estimates how long a job would take on a rental instance,
// based on historical launch data for the given GPU class. Returns nil if no
// historical data is available (e.g., first time using this GPU class).
func EstimateRentalCompletion(db *sql.DB, constraints Constraints, predict JobPredictor) *RentalEstimate {
	baseline := recentRentalBaseline(db, constraints.GPUClass)
	if baseline == nil {
		return nil
	}

	est := &RentalEstimate{
		CostPerHour: float64(baseline.CostPerHourCents) / 100.0,
	}

	// Launch overhead: startup + SSH + job setup (typical ~5-8 min)
	const defaultLaunchMin = 6.0
	est.LaunchEst = estimate.FromSeconds(
		defaultLaunchMin*60,
		3.0*60,  // p10: 3 min
		12.0*60, // p90: 12 min
	)

	// Transfer time for missing inputs (use rental download bandwidth)
	if db != nil && len(constraints.Inputs) > 0 {
		var totalMissingBytes int64
		for _, ref := range constraints.Inputs {
			asset, ok := dataloc.ParseAssetRef(ref)
			if !ok {
				continue
			}
			// For rental, all inputs are "missing" (fresh instance).
			// Use the largest known size from any host that has the asset.
			entries, err := dataloc.FindAssetHosts(db, asset)
			if err == nil {
				var maxSize int64
				for _, e := range entries {
					if e.SizeBytes > maxSize {
						maxSize = e.SizeBytes
					}
				}
				totalMissingBytes += maxSize
			}
		}
		if totalMissingBytes > 0 {
			bw := baseline.InetDownMbps * 1e6 / 8 // Mbps → bytes/sec
			if bw <= 0 {
				bw = 100e6 / 8 // default 100 Mbps
			}
			est.TransferEst = estimate.TransferTime(totalMissingBytes, bw)
		}
	}

	// Run time estimate
	const defaultRunMin = 60.0
	est.RunEst = estimate.FromSeconds(defaultRunMin*60, defaultRunMin*60*0.25, defaultRunMin*60*4.0)

	// If predictor available, use it (no contention on fresh rental)
	if predict != nil {
		// Use a synthetic host name — predictor may have GPU-class-level data
		if p := predict("rental:" + constraints.GPUClass); p != nil && p.DurationS != nil {
			est.RunEst = estimate.FromSeconds(*p.DurationS,
				safeDeref(p.DurationSLower, *p.DurationS*0.5),
				safeDeref(p.DurationSUpper, *p.DurationS*2.0))
		}
	}

	est.Total = est.LaunchEst.Add(est.TransferEst).Add(est.RunEst)

	slog.Debug("rental completion estimate",
		"gpu_class", constraints.GPUClass,
		"total_min", fmt.Sprintf("%.0f", est.Total.Mean.Minutes()),
		"launch_min", fmt.Sprintf("%.0f", est.LaunchEst.Mean.Minutes()),
		"transfer_min", fmt.Sprintf("%.0f", est.TransferEst.Mean.Minutes()),
		"run_min", fmt.Sprintf("%.0f", est.RunEst.Mean.Minutes()),
		"cost_per_hr", est.CostPerHour,
		"baseline_count", baseline.Count)

	return est
}

func safeDeref(p *float64, fallback float64) float64 {
	if p != nil {
		return *p
	}
	return fallback
}
