package campaign

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// calibPreClampThreshold is the observed production value of the unclamped
// learned threshold before the ceilings landed (ADR 0010). It is fixed here
// rather than recomputed from the survival curve.
const calibPreClampThreshold = 110 * time.Minute

// TestThresholdCalibration replays the recorded event times of every
// historical terminal launch against the current watchdog ceilings and
// reports how many healthy launches each rule would now destroy, and how
// much bleed each one prevents. It is an analysis tool, not an assertion:
// it is gated on WEFT_CALIBRATION_DB and skipped otherwise. The database is
// opened read-only and never written.
//
// The report is emitted through t.Logf and the test never fails, so it needs
// -v to show anything:
//
//	WEFT_CALIBRATION_DB=$HOME/.local/state/weft/jobs.db \
//	    go test ./internal/campaign/ -run TestThresholdCalibration -v
//
// Only two of the three ceilings are replayable. maxEmptyStatusTimeCeiling
// governs the wait for the provider to report any non-empty status. No column
// in `launches` records when that first happened — ready_at and
// agent_ready_at_unix are much later events, and provider_running_at is rule
// P's. provider_status_transitions rows with an empty old_status mark the
// provider becoming legible, but they are scoped to the observing process
// rather than to the launch: a restarted reconciler or a new watch session
// emits another one, so the earliest such row is a lower bound on latency, not
// a measurement of it. Scoring this ceiling still needs a per-launch
// first-legible timestamp, so it is left out rather than reported against a
// stand-in.
func TestThresholdCalibration(t *testing.T) {
	path := os.Getenv("WEFT_CALIBRATION_DB")
	if path == "" {
		t.Skip("set WEFT_CALIBRATION_DB=$HOME/.local/state/weft/jobs.db to run threshold calibration")
	}
	// mode=ro and no migrations, matching db.OpenReadOnly. busy_timeout matters
	// because the corpus is the live jobs.db, written concurrently by the
	// autopilot. sql.Open is lazy, so Ping is what turns a bad path into an
	// open error rather than a confusing one at query time.
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Fatalf("open db %s: %v", path, err)
	}

	rows, err := conn.Query(`
		SELECT launched_at, provider_running_at, first_onstart_probe_seen_unix,
		       agent_ready_at_unix, ended_at, provider, cost_per_hour_cents
		FROM launches
		WHERE status IN ('completed','failed','canceled') AND launched_at IS NOT NULL`)
	if err != nil {
		t.Fatalf("query launches: %v", err)
	}
	defer rows.Close()

	var launches []calibLaunch
	for rows.Next() {
		var l calibLaunch
		if err := rows.Scan(&l.launchedAt, &l.providerRunningAt, &l.firstProbeAt,
			&l.agentReadyAt, &l.endedAt, &l.provider, &l.costPerHourCents); err != nil {
			t.Fatalf("scan launch: %v", err)
		}
		launches = append(launches, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate launches: %v", err)
	}

	ready := 0
	byProvider := map[string][]calibLaunch{}
	for _, l := range launches {
		if l.agentReadyAt != nil {
			ready++
		}
		byProvider[l.provider] = append(byProvider[l.provider], l)
	}
	providers := make([]string, 0, len(byProvider))
	for p := range byProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	t.Logf("corpus: %d terminal launches with launched_at recorded, %d reached agent-ready",
		len(launches), ready)

	reportCalibRule(t, "P", "pre-running window: anchor launched_at, awaited provider_running_at",
		maxPreRunningStatusTimeCeiling, launches, providers, byProvider,
		func(l calibLaunch) *int64 { return &l.launchedAt },
		func(l calibLaunch) *int64 { return l.providerRunningAt })
	reportCalibRule(t, "D", "dud window: anchor provider_running_at, awaited first_onstart_probe_seen_unix",
		dudVastTimeoutCeiling, launches, providers, byProvider,
		func(l calibLaunch) *int64 { return l.providerRunningAt },
		func(l calibLaunch) *int64 { return l.firstProbeAt })
}

type calibLaunch struct {
	launchedAt        int64
	providerRunningAt *int64
	firstProbeAt      *int64
	agentReadyAt      *int64
	endedAt           *int64
	provider          string
	costPerHourCents  *int64
}

type calibStats struct {
	ineligible   int
	evaluable    int
	unevaluable  int
	wouldFire    int
	falseKills   int
	trueKills    int
	reclaimedSec int64
	reclaimedCts float64
	// unknownBleed counts true kills whose reclaimed rental cannot be priced,
	// because the launch has no end time or no recorded hourly rate. They are
	// unknown bleed, not zero bleed, so they travel alongside the dollar figure
	// instead of disappearing into it.
	unknownBleed int
}

// evaluateCalibWindow scores one rule at one threshold.
//
// The correctness trap: first_onstart_probe_seen_unix postdates much of the
// data, and provider_running_at has an analogous gap. A launch whose awaited
// event is unrecorded but which reached agent-ready did not fail to produce
// the event — it simply was not recorded. Such launches are unevaluable:
// counted separately and excluded from every rate.
//
// The anchors are observation times rather than event times: provider_running_at
// is stamped when the poller saw `running`, and the corpus holds launches whose
// OnStart probe precedes it. A late anchor shortens the measured window, so the
// false-kill rates below are floors, not point estimates.
func evaluateCalibWindow(ls []calibLaunch, ceiling time.Duration, anchor, event func(calibLaunch) *int64) calibStats {
	var s calibStats
	limit := int64(ceiling / time.Second)
	for _, l := range ls {
		a := anchor(l)
		if a == nil {
			s.ineligible++
			continue
		}
		e := event(l)
		ready := l.agentReadyAt != nil
		if e == nil && ready {
			s.unevaluable++
			continue
		}
		s.evaluable++
		fired := false
		if e != nil {
			fired = *e-*a > limit
		} else if l.endedAt != nil {
			fired = *l.endedAt-*a > limit
		}
		if !fired {
			continue
		}
		s.wouldFire++
		if ready {
			s.falseKills++
			continue
		}
		s.trueKills++
		if l.endedAt == nil {
			s.unknownBleed++
			continue
		}
		bleed := *l.endedAt - (*a + limit)
		if bleed <= 0 {
			continue
		}
		// Bleed seconds are knowable from the end time alone; only the price
		// needs the hourly rate. Gating both on the rate would drop knowable
		// hours out of the reclaim figure.
		s.reclaimedSec += bleed
		if l.costPerHourCents == nil {
			s.unknownBleed++
			continue
		}
		s.reclaimedCts += float64(bleed) * float64(*l.costPerHourCents) / 3600
	}
	return s
}

func reportCalibRule(t *testing.T, name, desc string, ceiling time.Duration, all []calibLaunch,
	providers []string, byProvider map[string][]calibLaunch, anchor, event func(calibLaunch) *int64) {
	t.Logf("rule %s — %s", name, desc)
	for _, p := range providers {
		ls := byProvider[p]
		cur := evaluateCalibWindow(ls, ceiling, anchor, event)
		pre := evaluateCalibWindow(ls, calibPreClampThreshold, anchor, event)
		logCalibStats(t, "    "+p+", current ceiling", ceiling, cur)
		logCalibStats(t, "    "+p+", pre-clamp (observed production value)", calibPreClampThreshold, pre)
	}
	overall := evaluateCalibWindow(all, ceiling, anchor, event)
	t.Logf("verdict — rule %s @ %s: %s false-kill over %d evaluable, reclaims %.1fh / %s",
		name, ceiling, overall.falseKillRate(), overall.evaluable,
		float64(overall.reclaimedSec)/3600, overall.reclaimedCost())
}

func logCalibStats(t *testing.T, label string, ceiling time.Duration, s calibStats) {
	t.Logf("%s @ %s: evaluable %d, unevaluable %d, ineligible %d, would-fire %d (false kills %d, true kills %d), false-kill %s, reclaims %.1fh / %s",
		label, ceiling, s.evaluable, s.unevaluable, s.ineligible, s.wouldFire,
		s.falseKills, s.trueKills, s.falseKillRate(),
		float64(s.reclaimedSec)/3600, s.reclaimedCost())
}

func (s calibStats) falseKillRate() string {
	if s.evaluable == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", 100*float64(s.falseKills)/float64(s.evaluable))
}

// reclaimedCost renders the reclaimed rental spend, naming any true kills whose
// bleed could not be priced so that the figure is never read as the whole of it.
func (s calibStats) reclaimedCost() string {
	if s.unknownBleed == 0 {
		return fmt.Sprintf("$%.2f", s.reclaimedCts/100)
	}
	return fmt.Sprintf("$%.2f + %d unpriced", s.reclaimedCts/100, s.unknownBleed)
}
