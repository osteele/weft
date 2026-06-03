package campaign

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// PriceAnchor summarizes what the user has implicitly authorized paying per
// hour for a (gpu_class, gpu_mem_gb) bucket. The anchor is derived from
// historical launches that actually came up — `launched_at IS NOT NULL` is the
// "user let this run" signal. Status-based filtering (running/grace/etc.) is
// intentionally avoided: if the launch reached the point of incurring cost,
// the user accepted that cost.
//
// We do NOT incorporate current market data into the anchor. Markets for
// rental GPUs are thin and can spike 10× on supply-constrained days; using
// market data as a ceiling-raising input would silently re-authorize spend
// the user has never agreed to. Market data is shown alongside the anchor
// for context only (see the `Market*` informational fields the gate code
// computes separately) and never widens the cap.
type PriceAnchor struct {
	GPUClass    string
	GPUMemGB    int
	SampleCount int
	WindowDays  int // 0 means the unbounded window was used
	P75Cents    int
	MedianCents int
	MinCents    int
	MaxCents    int
}

// PriceAnchorWindows is the backoff sequence used by PriceAnchorForClass.
// 30→90→365→unbounded, picking the smallest window that yields at least
// PriceAnchorMinSamples. Sparse classes (e.g. an H200 job we run every two
// months) reach back further automatically.
var PriceAnchorWindows = []int{30, 90, 365, 0}

// PriceAnchorMinSamples is the smallest number of historical launches at the
// same (gpu_class, gpu_mem_gb) bucket we'll accept before deciding "the user
// has an established price for this." Below this we return no anchor and the
// gate falls back to requiring explicit authorization — better than synthesizing
// one from a single data point.
const PriceAnchorMinSamples = 3

// PriceAnchorAutoApproveMultiplier is the headroom over the anchor that the
// gate auto-approves without prompting for authorization. Sized to absorb
// day-to-day price variance (which the p75 already partly captures) without
// silently sliding upward across price spikes.
const PriceAnchorAutoApproveMultiplier = 1.5

// PriceAnchorForClass returns the anchor for the given GPU class × VRAM
// bucket, or (nil, nil) if there isn't enough history in any window.
//
// Callers should treat (nil, nil) as "no anchor — require explicit
// authorization for any non-trivial spend in this class." See the gate code
// for how that signal feeds into placement_blocked.
func PriceAnchorForClass(database *sql.DB, gpuClass string, gpuMemGB int) (*PriceAnchor, error) {
	if database == nil || gpuClass == "" || gpuMemGB <= 0 {
		return nil, nil
	}
	for _, days := range PriceAnchorWindows {
		anchor, err := computePriceAnchor(database, gpuClass, gpuMemGB, days, time.Now())
		if err != nil {
			return nil, err
		}
		if anchor != nil && anchor.SampleCount >= PriceAnchorMinSamples {
			return anchor, nil
		}
	}
	return nil, nil
}

// computePriceAnchor is split out for testability with an injectable `now`.
func computePriceAnchor(database *sql.DB, gpuClass string, gpuMemGB int, windowDays int, now time.Time) (*PriceAnchor, error) {
	args := []interface{}{gpuClass, gpuMemGB}
	cutoffClause := ""
	if windowDays > 0 {
		cutoffClause = "AND launched_at >= ?"
		args = append(args, now.Add(-time.Duration(windowDays)*24*time.Hour).Unix())
	}
	query := fmt.Sprintf(`
		SELECT cost_per_hour_cents
		  FROM launches
		 WHERE gpu_class = ?
		   AND gpu_mem_gb = ?
		   AND cost_per_hour_cents > 0
		   AND launched_at IS NOT NULL
		   %s
		 ORDER BY cost_per_hour_cents`, cutoffClause)
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query launches for price anchor: %w", err)
	}
	defer rows.Close()
	var prices []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		prices = append(prices, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(prices) == 0 {
		return nil, nil
	}
	return &PriceAnchor{
		GPUClass:    gpuClass,
		GPUMemGB:    gpuMemGB,
		SampleCount: len(prices),
		WindowDays:  windowDays,
		P75Cents:    percentileInt(prices, 0.75),
		MedianCents: percentileInt(prices, 0.50),
		MinCents:    prices[0],
		MaxCents:    prices[len(prices)-1],
	}, nil
}

// AutoApproveCeilingCents is the upper bound the anchor allows without
// prompting the user — anchor's p75 scaled by the auto-approve multiplier.
func (a *PriceAnchor) AutoApproveCeilingCents() int {
	if a == nil {
		return 0
	}
	return int(float64(a.P75Cents) * PriceAnchorAutoApproveMultiplier)
}

// percentileInt returns the percentile-th value of a slice already sorted
// ascending. p in [0, 1]. Uses nearest-rank, which is fine for the small
// sample counts we work with here (typically <100).
func percentileInt(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	// Nearest-rank: ceil(p * N) - 1 (zero-indexed).
	idx := int(float64(len(sorted))*p+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// sortIntsAsc is a tiny helper to keep tests/callers honest about ordering.
// computePriceAnchor relies on ORDER BY in SQL, but callers constructing
// PriceAnchor in tests should pass already-sorted prices.
func sortIntsAsc(prices []int) {
	sort.Ints(prices)
}
