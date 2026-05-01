package bidding

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// ConcentrationCheckPeriod is the lookback window for the offer-pool
// concentration check at job submission time. We look at vast.ai
// launches the system has actually seen in this window as a stand-in for
// "the offer pool you're likely to get bids from"; older data is
// unrepresentative because vast's marketplace turns over quickly.
const ConcentrationCheckPeriod = 30 * 24 * time.Hour

// ConcentrationLowAbsolute and ConcentrationLowFraction are the
// thresholds at which the warning fires. The warning is suppressed only
// when both are comfortably exceeded: enough offers in absolute terms
// AND enough as a share of the family pool.
const (
	ConcentrationLowAbsolute = 10
	ConcentrationLowFraction = 0.10
)

// ConcentrationWarning describes why a job's constraints look likely to
// match a narrow slice of the offer pool. Empty Message means no warning.
type ConcentrationWarning struct {
	Message      string
	FamilyTotal  int     // observed launches in the family across the lookback window
	Candidates   int     // launches that would have satisfied the constraint
	TopRegion    string  // most common data_center among candidates
	TopRegionPct float64 // share of candidates in the top region (0-1)
}

// CheckOfferConcentration looks at the recent launch history (a stand-in
// for the live offer pool) for the requested GPU family, counts how many
// would have satisfied a `gpu_mem_gb >= requestedVRAMGB` constraint, and
// returns a warning when that count is too small in absolute terms or
// too small a fraction of the family pool.
//
// The warning is informational, not a hard error: there are good reasons
// to ask for an unusual SKU. But a 26 GB request that hits 8 of 340
// 4090 launches in 30 days is a foot-gun the user may not have known
// about — and naming the geographic concentration ("most candidates are
// in Sichuan, CN") explains *why* the placement will be cramped.
//
// Returns an empty ConcentrationWarning when the constraint isn't
// constrained enough to warrant analysis (no GPU class), when there's
// no historical data at all (cold start), or when the candidate pool is
// healthy.
func CheckOfferConcentration(db *sql.DB, gpuClass string, requestedVRAMGB int) (ConcentrationWarning, error) {
	if db == nil {
		return ConcentrationWarning{}, nil
	}
	gpuClass = strings.TrimSpace(gpuClass)
	if gpuClass == "" {
		return ConcentrationWarning{}, nil
	}
	familyPattern := familyMatchPattern(gpuClass)
	if familyPattern == "" {
		return ConcentrationWarning{}, nil
	}

	since := time.Now().Add(-ConcentrationCheckPeriod).Unix()
	rows, err := db.Query(`
		SELECT COALESCE(gpu_mem_gb, 0), COALESCE(data_center, '')
		FROM launches
		WHERE provider = 'vastai'
		  AND status IN ('completed', 'failed', 'canceled')
		  AND COALESCE(ended_at, 0) >= ?
		  AND resolved_gpu_name LIKE ?
	`, since, "%"+familyPattern+"%")
	if err != nil {
		return ConcentrationWarning{}, fmt.Errorf("query offer concentration: %w", err)
	}
	defer rows.Close()

	familyTotal := 0
	candidates := 0
	regionCounts := make(map[string]int)
	for rows.Next() {
		var vram int
		var dc string
		if err := rows.Scan(&vram, &dc); err != nil {
			return ConcentrationWarning{}, err
		}
		familyTotal++
		if requestedVRAMGB > 0 && vram < requestedVRAMGB {
			continue
		}
		candidates++
		if dc != "" {
			regionCounts[dc]++
		}
	}
	if err := rows.Err(); err != nil {
		return ConcentrationWarning{}, err
	}

	if familyTotal == 0 {
		// No history: either a cold start or a brand-new family. Stay
		// silent rather than warn from absent evidence.
		return ConcentrationWarning{}, nil
	}

	fraction := 0.0
	if familyTotal > 0 {
		fraction = float64(candidates) / float64(familyTotal)
	}

	if candidates >= ConcentrationLowAbsolute && fraction >= ConcentrationLowFraction {
		return ConcentrationWarning{}, nil
	}

	topRegion, topRegionPct := dominantRegion(regionCounts, candidates)
	pct := math.Round(fraction*1000) / 10
	days := int(ConcentrationCheckPeriod / (24 * time.Hour))
	msg := fmt.Sprintf(
		"only %d of %d %s launches in the last %d days would have matched (%.1f%%); expect concentrated placement",
		candidates, familyTotal, gpuClass, days, pct,
	)
	if topRegion != "" && topRegionPct >= 0.5 {
		msg += fmt.Sprintf(" — top region: %s (%.0f%% of candidates)", topRegion, topRegionPct*100)
	}
	return ConcentrationWarning{
		Message:      msg,
		FamilyTotal:  familyTotal,
		Candidates:   candidates,
		TopRegion:    topRegion,
		TopRegionPct: topRegionPct,
	}, nil
}

// familyMatchPattern reduces a user-facing gpu_class string to its
// leading family token for substring matching against
// resolved_gpu_name. e.g. "a100-sxm4-80gb" → "a100", "rtx-4090" →
// "rtx", "4090" → "4090". The caller wraps this in `%…%` for a
// case-insensitive LIKE query (vast resolves to "RTX 4090", "A100
// SXM", etc.).
//
// For multi-token families like "rtx 4090", we'd ideally keep both
// tokens, but the model number (4090) alone is unique enough across
// the vast catalogue that the simpler form does the right thing.
func familyMatchPattern(gpuClass string) string {
	g := strings.TrimSpace(gpuClass)
	g = strings.ReplaceAll(g, "_", "-")
	parts := strings.Split(g, "-")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Skip generic prefixes that aren't useful for matching the
		// resolved name; the next token is the model.
		if eq := strings.ToLower(p); eq == "rtx" || eq == "gtx" || eq == "tesla" || eq == "nvidia" || eq == "" {
			continue
		}
		return p
	}
	return ""
}

// dominantRegion returns the most frequent region and its share of
// candidates. Returns ("", 0) when no region is clearly dominant.
func dominantRegion(counts map[string]int, total int) (string, float64) {
	best := ""
	bestN := 0
	for region, n := range counts {
		if n > bestN {
			bestN = n
			best = region
		}
	}
	if total == 0 {
		return "", 0
	}
	return best, float64(bestN) / float64(total)
}
