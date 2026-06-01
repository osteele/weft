// Package credit detects retroactive credit-exhaustion incidents on cloud
// providers by looking for mass-destroy bursts followed by a silent gap
// (no instance reached "running") until a recovery launch comes up.
//
// The detector is provider-specific: a burst on one provider must be matched
// against silence and recovery on that same provider. This avoids cross-
// provider false matches that would otherwise need operator review.
package credit

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/osteele/weft/internal/db"
)

// Defaults for the detector tunables. Selected to match observed Vast.ai
// behavior on prior incidents:
//   - BurstWindow=8m is wider than the existing CreateInstance signature
//     cluster (~5m) to absorb Vast's actual destruction pace.
//   - LeftPad=15m matches creditClusterBeforeWindow in mark-credit-exhausted;
//     it catches running rentals that Vast destroys before the explicit
//     CreateInstance failures begin.
//   - MinSilence=90s rejects bursts followed by a quick same-provider
//     recovery, which usually indicates a regional outage rather than a
//     wallet event.
const (
	DefaultLookback    = 7 * 24 * time.Hour
	DefaultBurstMin    = 3
	DefaultBurstWindow = 8 * time.Minute
	DefaultLeftPad     = 15 * time.Minute
	DefaultMinSilence  = 90 * time.Second
)

// Config tunes the detector. Zero values fall back to the Default* constants.
type Config struct {
	Lookback    time.Duration
	BurstMin    int
	BurstWindow time.Duration
	LeftPad     time.Duration
	MinSilence  time.Duration
}

func (c Config) withDefaults() Config {
	if c.Lookback <= 0 {
		c.Lookback = DefaultLookback
	}
	if c.BurstMin <= 0 {
		c.BurstMin = DefaultBurstMin
	}
	if c.BurstWindow <= 0 {
		c.BurstWindow = DefaultBurstWindow
	}
	if c.LeftPad <= 0 {
		c.LeftPad = DefaultLeftPad
	}
	if c.MinSilence <= 0 {
		c.MinSilence = DefaultMinSilence
	}
	return c
}

// Incident describes a detected credit-exhaustion event on a specific
// provider. Members are sorted newest-first by EndedAt.
type Incident struct {
	Provider       string
	BurstStart     time.Time
	BurstEnd       time.Time
	BurstCount     int           // number of failures in the burst itself (not the full cluster)
	Recovery       time.Time     // first same-provider running after burst (or detection time)
	RecoveryLaunch *db.Launch    // nil if Ongoing
	Ongoing        bool          // true if no same-provider running observed since the burst
	WindowStart    time.Time     // BurstStart - LeftPad
	WindowEnd      time.Time     // Recovery
	Silence        time.Duration // Recovery - BurstEnd (>= MinSilence; 0 if Ongoing and lookback short)
	Members        []*db.Launch  // all eligible failures in [WindowStart, WindowEnd) on this provider
}

// Rejection records a candidate burst the detector considered but discarded.
// Surfaced for diagnostics so the operator understands why "no incident"
// was returned despite a visible cluster of failures.
type Rejection struct {
	Provider   string
	BurstEnd   time.Time
	BurstCount int
	Reason     string // "regional_outage" | "short_silence"
	Detail     string // human-readable explanation
}

// Result is what Detect returns. Incident is the chosen detection (most
// recent qualifying burst across providers, or nil if none). Rejections
// records any candidate bursts that failed the safety checks.
type Result struct {
	Incident   *Incident
	Rejections []Rejection
}

// Detect runs the credit-exhaustion incident detector over the supplied
// failures (eligible failed/canceled launches with non-nil EndedAt) and
// runningSignals (launches with non-nil ProviderRunningAt). It returns the
// most recent qualifying incident across all providers, or nil.
//
// The detector is pure: callers prepare the inputs (typically via
// DetectFromDB) and pass `now` so tests can use a deterministic clock.
func Detect(cfg Config, failures, runningSignals []*db.Launch, now time.Time) *Result {
	cfg = cfg.withDefaults()
	result := &Result{}

	failuresByProvider := groupByProvider(failures, func(l *db.Launch) (int64, bool) {
		if l.EndedAt == nil {
			return 0, false
		}
		return *l.EndedAt, true
	})
	runningByProvider := groupByProvider(runningSignals, func(l *db.Launch) (int64, bool) {
		if l.ProviderRunningAt == nil {
			return 0, false
		}
		return *l.ProviderRunningAt, true
	})

	var candidates []*Incident
	providers := make([]string, 0, len(failuresByProvider))
	for p := range failuresByProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers) // deterministic iteration for tests

	for _, provider := range providers {
		incident, rejections := detectForProvider(cfg, provider,
			failuresByProvider[provider], runningByProvider[provider], now)
		result.Rejections = append(result.Rejections, rejections...)
		if incident != nil {
			candidates = append(candidates, incident)
		}
	}
	if len(candidates) > 0 {
		// Pick the most recent burst across providers.
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].BurstEnd.After(candidates[j].BurstEnd)
		})
		result.Incident = candidates[0]
	}
	return result
}

// detectForProvider runs the algorithm on a single provider's failures and
// running signals. Failures are pre-filtered to those with EndedAt set;
// runningTimes are pre-filtered to those with ProviderRunningAt set.
//
// Returns the most recent qualifying burst on this provider plus any
// rejected candidates encountered while searching.
func detectForProvider(cfg Config, provider string, failures, runningTimes []*db.Launch, now time.Time) (*Incident, []Rejection) {
	if len(failures) < cfg.BurstMin {
		return nil, nil
	}
	// Sort newest-first by EndedAt; running times oldest-first for lookup.
	sort.Slice(failures, func(i, j int) bool { return *failures[i].EndedAt > *failures[j].EndedAt })
	sort.Slice(runningTimes, func(i, j int) bool {
		return *runningTimes[i].ProviderRunningAt < *runningTimes[j].ProviderRunningAt
	})

	burstWindowSecs := int64(cfg.BurstWindow.Seconds())
	minSilenceSecs := int64(cfg.MinSilence.Seconds())
	leftPadSecs := int64(cfg.LeftPad.Seconds())

	var rejections []Rejection

	// Walk newest→oldest. For each anchor i, the candidate burst is failures
	// [i:j) where j is the smallest index with *failures[j].EndedAt <
	// (*failures[i].EndedAt - burstWindow). If j-i >= BurstMin we evaluate
	// the candidate; on rejection, we advance past it (i = j) to avoid
	// re-evaluating overlapping windows.
	i := 0
	for i < len(failures) {
		anchor := failures[i]
		anchorEnd := *anchor.EndedAt
		j := i + 1
		for j < len(failures) && anchorEnd-*failures[j].EndedAt <= burstWindowSecs {
			j++
		}
		if j-i < cfg.BurstMin {
			i++
			continue
		}
		burst := failures[i:j]
		burstStart := *burst[len(burst)-1].EndedAt
		burstEnd := anchorEnd

		// Survivor check (stronger than regional-outage): if any
		// same-provider launch was running before the burst started and
		// either is still alive or ended after the burst, the wallet was
		// clearly funded throughout — Vast destroys *all* running rentals
		// when the balance hits zero, so a survivor disproves the wallet
		// theory. Run this first because it's the most definitive signal.
		if survivor := findSurvivorThroughBurst(runningTimes, burstStart, burstEnd); survivor != nil {
			endedDesc := "still alive"
			if survivor.EndedAt != nil {
				endedDesc = "ended " + time.Unix(*survivor.EndedAt, 0).UTC().Format(time.RFC3339)
			}
			rejections = append(rejections, Rejection{
				Provider:   provider,
				BurstEnd:   time.Unix(burstEnd, 0).UTC(),
				BurstCount: len(burst),
				Reason:     "regional_outage",
				Detail: fmt.Sprintf("instance %d was running before the burst (started %s, %s) — wallet was funded throughout, looks like a regional outage on %s",
					survivor.ID, time.Unix(*survivor.ProviderRunningAt, 0).UTC().Format(time.RFC3339), endedDesc, provider),
			})
			i = j
			continue
		}

		// Recovery: first same-provider running after burstEnd.
		var recoveryTS int64
		var recoveryLaunch *db.Launch
		ongoing := false
		recoveryIdx := firstRunningAfter(runningTimes, burstEnd)
		if recoveryIdx < 0 {
			ongoing = true
			recoveryTS = now.Unix()
		} else {
			recoveryLaunch = runningTimes[recoveryIdx]
			recoveryTS = *recoveryLaunch.ProviderRunningAt
		}

		// Regional outage check: any same-provider launch reached running
		// during [burstStart, recoveryTS). The survivor check above
		// covered launches that started before the burst; this covers
		// launches that came up DURING the silence window.
		if violator := findRunningInRange(runningTimes, burstStart, recoveryTS); violator != nil {
			rejections = append(rejections, Rejection{
				Provider:   provider,
				BurstEnd:   time.Unix(burstEnd, 0).UTC(),
				BurstCount: len(burst),
				Reason:     "regional_outage",
				Detail: fmt.Sprintf("instance %d reached running at %s during the candidate window — looks like a regional outage on %s, not a wallet event",
					violator.ID, time.Unix(*violator.ProviderRunningAt, 0).UTC().Format(time.RFC3339), provider),
			})
			i = j
			continue
		}

		// Silence check.
		silenceSecs := recoveryTS - burstEnd
		if !ongoing && silenceSecs < minSilenceSecs {
			rejections = append(rejections, Rejection{
				Provider:   provider,
				BurstEnd:   time.Unix(burstEnd, 0).UTC(),
				BurstCount: len(burst),
				Reason:     "short_silence",
				Detail: fmt.Sprintf("recovery on %s came %s after burst — below the %s floor; likely a transient cluster, not credit exhaustion",
					provider, time.Duration(silenceSecs)*time.Second, cfg.MinSilence),
			})
			i = j
			continue
		}
		if ongoing && now.Unix()-burstEnd < minSilenceSecs {
			// Burst may still be in progress; refuse to declare an incident
			// until we've waited out MinSilence. No rejection logged — this
			// isn't a discriminator failure, just "wait and rerun".
			i = j
			continue
		}

		// Build cluster: all same-provider failures with EndedAt in
		// [burstStart - LeftPad, recoveryTS).
		windowStart := burstStart - leftPadSecs
		windowEnd := recoveryTS
		members := []*db.Launch{}
		for _, f := range failures {
			t := *f.EndedAt
			if t >= windowStart && t < windowEnd {
				members = append(members, f)
			}
		}
		// Match sortLaunchesNewestFirst in cmd/instance_mark_credit_exhausted.go
		// so --auto and --since preview rows in the same order when EndedAt
		// is identical (common in a tight burst).
		sort.Slice(members, func(a, b int) bool {
			ai, aj := *members[a].EndedAt, *members[b].EndedAt
			if ai != aj {
				return ai > aj
			}
			return members[a].ID > members[b].ID
		})

		return &Incident{
			Provider:       provider,
			BurstStart:     time.Unix(burstStart, 0).UTC(),
			BurstEnd:       time.Unix(burstEnd, 0).UTC(),
			BurstCount:     len(burst),
			Recovery:       time.Unix(recoveryTS, 0).UTC(),
			RecoveryLaunch: recoveryLaunch,
			Ongoing:        ongoing,
			WindowStart:    time.Unix(windowStart, 0).UTC(),
			WindowEnd:      time.Unix(windowEnd, 0).UTC(),
			Silence:        time.Duration(silenceSecs) * time.Second,
			Members:        members,
		}, rejections
	}
	return nil, rejections
}

// firstRunningAfter returns the index of the earliest running signal with
// ProviderRunningAt > t, or -1 if none. runningTimes must be sorted oldest-
// first.
func firstRunningAfter(runningTimes []*db.Launch, t int64) int {
	idx := sort.Search(len(runningTimes), func(i int) bool {
		return *runningTimes[i].ProviderRunningAt > t
	})
	if idx >= len(runningTimes) {
		return -1
	}
	return idx
}

// findSurvivorThroughBurst returns the first running signal that was
// alive throughout [burstStart, burstEnd]: it reached running at or
// before burstStart AND either is still alive (EndedAt == nil) or
// ended after burstEnd. Such a launch is conclusive proof the wallet
// was funded during the burst — Vast destroys every running rental
// when credit hits zero, so a survivor disproves credit exhaustion.
// runningTimes must be sorted oldest-first.
func findSurvivorThroughBurst(runningTimes []*db.Launch, burstStart, burstEnd int64) *db.Launch {
	for _, r := range runningTimes {
		if *r.ProviderRunningAt > burstStart {
			// Sorted oldest-first; once running-at passes burstStart,
			// no later entry can be a survivor.
			return nil
		}
		if r.EndedAt == nil || *r.EndedAt > burstEnd {
			return r
		}
	}
	return nil
}

// findRunningInRange returns the first running signal with timestamp in
// [start, end), or nil if none. runningTimes must be sorted oldest-first.
func findRunningInRange(runningTimes []*db.Launch, start, end int64) *db.Launch {
	idx := sort.Search(len(runningTimes), func(i int) bool {
		return *runningTimes[i].ProviderRunningAt >= start
	})
	if idx >= len(runningTimes) {
		return nil
	}
	if *runningTimes[idx].ProviderRunningAt < end {
		return runningTimes[idx]
	}
	return nil
}

// groupByProvider partitions launches by their Provider field, skipping any
// launch where keyFn reports no usable timestamp.
func groupByProvider(launches []*db.Launch, keyFn func(*db.Launch) (int64, bool)) map[string][]*db.Launch {
	groups := make(map[string][]*db.Launch)
	for _, l := range launches {
		if _, ok := keyFn(l); !ok {
			continue
		}
		if l.Provider == "" {
			continue
		}
		groups[l.Provider] = append(groups[l.Provider], l)
	}
	return groups
}

// DetectFromDB queries the database for the inputs Detect needs and runs
// the detector. Lookback comes from cfg (or DefaultLookback if zero).
func DetectFromDB(database *sql.DB, cfg Config, now time.Time) (*Result, error) {
	cfg = cfg.withDefaults()
	sinceUnix := now.Add(-cfg.Lookback).Unix()

	failures, err := db.ListReclassifyEligibleLaunches(database, sinceUnix)
	if err != nil {
		return nil, fmt.Errorf("list eligible failures: %w", err)
	}
	runningSignals, err := listRunningSignals(database, sinceUnix)
	if err != nil {
		return nil, fmt.Errorf("list running signals: %w", err)
	}
	// Drop running signals from launches that are themselves eligible
	// failures: a credit victim that briefly reached running before Vast
	// destroyed it would otherwise self-trigger the regional-outage check
	// and suppress its own incident. Failures with non-eligible reasons
	// (job_failure, disk_full, weft_bug, etc.) are real machine signals
	// and remain trusted.
	eligibleFailureIDs := make(map[int64]struct{}, len(failures))
	for _, f := range failures {
		eligibleFailureIDs[f.ID] = struct{}{}
	}
	trusted := runningSignals[:0]
	for _, r := range runningSignals {
		if _, victim := eligibleFailureIDs[r.ID]; victim {
			continue
		}
		trusted = append(trusted, r)
	}
	return Detect(cfg, failures, trusted, now), nil
}

// listRunningSignals returns all launches whose ProviderRunningAt is set
// and either the launch is still alive or it ended at/after sinceUnix.
// EndedAt is included so the survivor check can tell whether a launch
// was actively running through a burst window. DetectFromDB filters out
// launches that are themselves eligible failures (potential credit
// victims) before passing the list to Detect.
func listRunningSignals(database *sql.DB, sinceUnix int64) ([]*db.Launch, error) {
	rows, err := database.Query(`SELECT id, provider, provider_running_at, ended_at
		FROM launches
		WHERE provider_running_at IS NOT NULL
		  AND (ended_at IS NULL OR ended_at >= ?)
		ORDER BY provider_running_at`, sinceUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*db.Launch
	for rows.Next() {
		l := &db.Launch{}
		var runAt int64
		var endedAt sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Provider, &runAt, &endedAt); err != nil {
			return nil, err
		}
		l.ProviderRunningAt = &runAt
		if endedAt.Valid {
			v := endedAt.Int64
			l.EndedAt = &v
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
