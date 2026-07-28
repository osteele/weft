package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/retrypolicy"
)

// PriceGateContext is the per-launch state the gate needs to decide whether a
// candidate offer is within the user's authorized spend. It's narrow on
// purpose: only the bucket (gpu_class × gpu_mem_gb), the set of jobs whose
// per-job overrides apply, and a DB handle to read the anchor and any
// authorizations.
type PriceGateContext struct {
	GPUClass string
	GPUMemGB int
	JobIDs   []int64
	DB       *sql.DB
}

// PriceGateDecision captures how the gate evaluated one candidate offer. The
// `Approved` bit is the load-bearing part; the rest is context the caller
// surfaces in placement_reasons when authorization is required.
type PriceGateDecision struct {
	Approved          bool
	OfferedCents      int
	AnchorCents       int // 0 if no anchor (sparse class history)
	CeilingCents      int // 0 if no ceiling was determinable
	Source            string
	AnchorSamples     int
	AnchorWindowDays  int
	MarketMedianCents int // informational only; never enters the gate logic
}

// PriceAuthorizationRequiredError is the sentinel error returned when the
// gate rejects every replacement offer in the search budget. It carries
// enough structured detail that `placement_blocked` can render a useful
// breakdown: offered, anchor, ceiling, and the (informational) market
// median.
type PriceAuthorizationRequiredError struct {
	GPUClass          string
	GPUMemGB          int
	OfferedCents      int
	AnchorCents       int
	CeilingCents      int
	MarketMedianCents int
	AnchorSamples     int
	AnchorWindowDays  int
}

func (e *PriceAuthorizationRequiredError) Error() string {
	if e.CeilingCents > 0 {
		return fmt.Sprintf(
			"requires price authorization: offered $%.2f/hr exceeds your authorized $%.2f/hr for %s ≥%dGB (anchor $%.2f/hr from %d samples in last %dd; current market median $%.2f/hr)",
			centsPerHourFloat(e.OfferedCents),
			centsPerHourFloat(e.CeilingCents),
			e.GPUClass, e.GPUMemGB,
			centsPerHourFloat(e.AnchorCents),
			e.AnchorSamples, e.AnchorWindowDays,
			centsPerHourFloat(e.MarketMedianCents),
		)
	}
	return fmt.Sprintf(
		"requires price authorization: no history yet for %s ≥%dGB; offered $%.2f/hr (current market median $%.2f/hr)",
		e.GPUClass, e.GPUMemGB,
		centsPerHourFloat(e.OfferedCents),
		centsPerHourFloat(e.MarketMedianCents),
	)
}

// IsPriceAuthorizationRequired tests whether err is the gate's authorization-
// required sentinel. Use this in the launch path to convert into a structured
// `placement_blocked: requires_price_authorization` rather than treating the
// failure as a generic infra error.
func IsPriceAuthorizationRequired(err error) bool {
	var target *PriceAuthorizationRequiredError
	return errors.As(err, &target)
}

// evaluatePriceGate returns whether `offer` is within the user's authorized
// spend for the given context. The decision uses three sources in priority
// order:
//
//  1. Any per-job authorization (`jobs.price_authorized_up_to_cents`) for the
//     jobs in this group — the max wins, because a single authorized job
//     effectively raises the gate for the instance it triggers.
//  2. A class-level sticky authorization
//     (`price_authorizations(gpu_class, gpu_mem_gb)`).
//  3. The history-derived anchor's auto-approve ceiling
//     (PriceAnchor.AutoApproveCeilingCents). When no anchor is available
//     (sparse class history), there is no implicit ceiling and explicit
//     authorization is required.
//
// The function is intentionally pure aside from the SQL reads — it does not
// mutate placement_reasons or write any kind of audit row. Surfacing is the
// caller's job.
func evaluatePriceGate(ctx PriceGateContext, offer cloud.Offer) PriceGateDecision {
	dec := PriceGateDecision{
		OfferedCents: int(offer.CostPerHour*100 + 0.5),
	}
	if ctx.DB == nil || ctx.GPUClass == "" || ctx.GPUMemGB <= 0 || offer.CostPerHour <= 0 {
		// Cannot evaluate without the bucket. Fail open: approve the offer
		// rather than silently block a launch on missing context. The
		// surrounding code is responsible for not calling the gate when
		// it isn't ready.
		dec.Approved = true
		dec.Source = "no-context"
		return dec
	}

	if ceil, src := perJobCeiling(ctx); ceil > 0 {
		dec.CeilingCents = ceil
		dec.Source = src
		dec.Approved = dec.OfferedCents <= ceil
		return dec
	}
	if ceil, ok, err := db.GetClassPriceAuthorization(ctx.DB, ctx.GPUClass, ctx.GPUMemGB); err == nil && ok && ceil > 0 {
		dec.CeilingCents = ceil
		dec.Source = "class-authorization"
		dec.Approved = dec.OfferedCents <= ceil
		return dec
	}

	anchor, err := PriceAnchorForClass(ctx.DB, ctx.GPUClass, ctx.GPUMemGB)
	if err != nil {
		slog.Warn("price anchor lookup failed", "component", "price-gate", "gpu_class", ctx.GPUClass, "gpu_mem_gb", ctx.GPUMemGB, "error", err)
		// Fail open — a transient SQL error must not block legitimate
		// launches. The gate is a guardrail, not a hard correctness
		// boundary.
		dec.Approved = true
		dec.Source = "lookup-error"
		return dec
	}
	if anchor == nil {
		// No history → no implicit authorization. The caller renders this
		// as "no history yet" in the placement_blocked message.
		dec.Source = "no-anchor"
		return dec
	}
	dec.AnchorCents = anchor.P75Cents
	dec.AnchorSamples = anchor.SampleCount
	dec.AnchorWindowDays = anchor.WindowDays
	dec.CeilingCents = anchor.AutoApproveCeilingCents()
	dec.Source = "anchor"
	dec.Approved = dec.OfferedCents <= dec.CeilingCents
	return dec
}

// perJobCeiling returns the maximum per-job authorization across the jobs in
// the context. Zero means "no per-job override is set on any job here." A
// non-zero result wins over class-level and anchor-based ceilings.
//
// Multi-job groups: we take the MAX rather than the MIN to keep the
// permissive semantics — if any one job's user explicitly said "spend up to
// $X for this," that authorization carries through to the instance the
// group lands on.
func perJobCeiling(ctx PriceGateContext) (int, string) {
	if ctx.DB == nil || len(ctx.JobIDs) == 0 {
		return 0, ""
	}
	maxCents := 0
	for _, jobID := range ctx.JobIDs {
		cents, ok, err := db.GetJobPriceAuthorization(ctx.DB, jobID)
		if err != nil {
			slog.Warn("per-job price authorization lookup failed", "component", "price-gate", "job_id", jobID, "error", err)
			continue
		}
		if ok && cents > maxCents {
			maxCents = cents
		}
	}
	if maxCents > 0 {
		return maxCents, "per-job-authorization"
	}
	return 0, ""
}

// searchAuthorizedReplacementOffer replaces the old
// searchReplacementOfferWithPriceCap. It repeatedly asks the inner search
// for a replacement offer, runs the price gate against each, and returns
// the first approved one. If the search budget is exhausted with every
// candidate rejected by the gate, the function returns a
// PriceAuthorizationRequiredError carrying the last evaluated decision so
// the caller can surface it as a structured authorization-required block.
//
// Note that this gate applies uniformly to every replacement candidate
// regardless of whether the candidate's provider differs from the original
// offer's provider. The cross-provider distinction is not meaningful at the
// authorization level — the user authorizes a spend ceiling, not a provider
// boundary.
func searchAuthorizedReplacementOffer(
	gateCtx PriceGateContext,
	excludeOfferKeys map[string]struct{},
	search func(map[string]struct{}) GroupOffer,
) (*cloud.Offer, error) {
	var lastDecision PriceGateDecision
	var candidates []cloud.Offer
	for range retrypolicy.MaxReplacementOfferSearchAttempts() {
		replacement := search(excludeOfferKeys)
		if replacement.Err != nil {
			return nil, replacement.Err
		}
		if replacement.Offer == nil {
			break
		}
		candidates = append(candidates, *replacement.Offer)
		decision := evaluatePriceGate(gateCtx, *replacement.Offer)
		if decision.Approved {
			return replacement.Offer, nil
		}
		lastDecision = decision
		excludeOfferKeys[replacement.Offer.Key()] = struct{}{}
	}
	if lastDecision.OfferedCents > 0 {
		_, marketMedianCents, _ := OfferPriceQuantilesCents(candidates)
		return nil, &PriceAuthorizationRequiredError{
			GPUClass:          gateCtx.GPUClass,
			GPUMemGB:          gateCtx.GPUMemGB,
			OfferedCents:      lastDecision.OfferedCents,
			AnchorCents:       lastDecision.AnchorCents,
			CeilingCents:      lastDecision.CeilingCents,
			MarketMedianCents: marketMedianCents,
			AnchorSamples:     lastDecision.AnchorSamples,
			AnchorWindowDays:  lastDecision.AnchorWindowDays,
		}
	}
	return nil, nil
}

// centsPerHourFloat converts an integer cents/hour value to a float for
// rendering. Centralized so the rounding behavior stays consistent across
// every surface that quotes the gate's numbers.
func centsPerHourFloat(cents int) float64 {
	return float64(cents) / 100.0
}

// groupJobIDs extracts the job IDs of an instance group. Nil-safe; jobs
// without IDs (test fixtures) are skipped.
func groupJobIDs(group InstanceGroup) []int64 {
	ids := make([]int64, 0, len(group.Jobs))
	for _, j := range group.Jobs {
		if j == nil || j.ID == 0 {
			continue
		}
		ids = append(ids, j.ID)
	}
	return ids
}
