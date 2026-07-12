package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func insertAnchorHistory(t *testing.T, database *sql.DB, gpuClass string, gpuMemGB int, costsCents ...int) {
	t.Helper()
	now := time.Now()
	for i, c := range costsCents {
		insertHistoricalLaunch(t, database, gpuClass, gpuMemGB, c, now.AddDate(0, 0, -1-i))
	}
}

func makeOffer(costPerHour float64) cloud.Offer {
	return cloud.Offer{ProviderID: "test-offer", CostPerHour: costPerHour}
}

func TestPriceGate_ApprovesWithinAnchorCeiling(t *testing.T) {
	database := db.SetupTestDB(t)
	insertAnchorHistory(t, database, "A100", 80, 100, 110, 120, 130)
	// p75 ≈ 130; auto-approve ceiling = 130 * 1.5 = 195.
	ctx := PriceGateContext{GPUClass: "A100", GPUMemGB: 80, DB: database}

	dec := evaluatePriceGate(ctx, makeOffer(1.50)) // 150 cents/hr
	if !dec.Approved {
		t.Fatalf("expected approval at $1.50/hr under ~$1.95/hr ceiling; got %+v", dec)
	}
	if dec.Source != "anchor" {
		t.Errorf("Source = %q, want anchor", dec.Source)
	}
}

func TestPriceGate_RejectsAboveAnchorCeiling(t *testing.T) {
	database := db.SetupTestDB(t)
	insertAnchorHistory(t, database, "A100", 80, 100, 110, 120, 130)
	ctx := PriceGateContext{GPUClass: "A100", GPUMemGB: 80, DB: database}

	dec := evaluatePriceGate(ctx, makeOffer(5.00)) // way above 195
	if dec.Approved {
		t.Fatalf("expected rejection at $5.00/hr above ~$1.95/hr ceiling; got %+v", dec)
	}
	if dec.CeilingCents == 0 {
		t.Errorf("CeilingCents = 0, want ceiling derived from anchor")
	}
}

func TestPriceGate_NoAnchorRequiresAuthorization(t *testing.T) {
	database := db.SetupTestDB(t)
	// No history for this bucket → no anchor, no class auth, no per-job auth.
	ctx := PriceGateContext{GPUClass: "H200", GPUMemGB: 141, DB: database}

	dec := evaluatePriceGate(ctx, makeOffer(2.50))
	if dec.Approved {
		t.Fatalf("expected rejection with no anchor / no overrides; got %+v", dec)
	}
	if dec.Source != "no-anchor" {
		t.Errorf("Source = %q, want no-anchor", dec.Source)
	}
}

func TestPriceGate_ClassAuthorizationWinsOverAnchor(t *testing.T) {
	database := db.SetupTestDB(t)
	insertAnchorHistory(t, database, "A100", 80, 100, 110, 120, 130)
	// Anchor ceiling ~195. Class auth at 300 — should approve up to that.
	if err := db.UpsertClassPriceAuthorization(database, "A100", 80, 300, "test", ""); err != nil {
		t.Fatalf("upsert class auth: %v", err)
	}
	ctx := PriceGateContext{GPUClass: "A100", GPUMemGB: 80, DB: database}

	dec := evaluatePriceGate(ctx, makeOffer(2.80)) // 280 — above anchor ceiling, under class auth
	if !dec.Approved {
		t.Fatalf("expected class auth to approve $2.80/hr (ceiling $3.00); got %+v", dec)
	}
	if dec.Source != "class-authorization" {
		t.Errorf("Source = %q, want class-authorization", dec.Source)
	}
}

func TestPriceGate_PerJobAuthorizationWinsOverEverything(t *testing.T) {
	database := db.SetupTestDB(t)
	insertAnchorHistory(t, database, "A100", 80, 100, 110, 120, 130)
	if err := db.UpsertClassPriceAuthorization(database, "A100", 80, 300, "test", ""); err != nil {
		t.Fatalf("upsert class auth: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", "/tmp", "true", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobPriceAuthorization(database, jobID, 500); err != nil {
		t.Fatalf("set per-job auth: %v", err)
	}
	ctx := PriceGateContext{GPUClass: "A100", GPUMemGB: 80, JobIDs: []int64{jobID}, DB: database}

	dec := evaluatePriceGate(ctx, makeOffer(4.90)) // 490 — above class auth, under per-job
	if !dec.Approved {
		t.Fatalf("expected per-job auth to approve $4.90/hr (ceiling $5.00); got %+v", dec)
	}
	if dec.Source != "per-job-authorization" {
		t.Errorf("Source = %q, want per-job-authorization", dec.Source)
	}
}

func TestSearchAuthorizedReplacementOffer_ReturnsErrorWhenAllRejected(t *testing.T) {
	database := db.SetupTestDB(t)
	insertAnchorHistory(t, database, "A100", 80, 100, 110, 120, 130)
	ctx := PriceGateContext{GPUClass: "A100", GPUMemGB: 80, DB: database}

	costs := []float64{9.00, 10.00, 11.00}
	calls := 0
	search := func(_ map[string]struct{}) GroupOffer {
		if calls >= len(costs) {
			return GroupOffer{}
		}
		cost := costs[calls]
		calls++
		// Every offer is way over the auto-approve ceiling.
		return GroupOffer{Offer: &cloud.Offer{ProviderID: fmt.Sprintf("offer-%d", calls), CostPerHour: cost}}
	}

	exclude := make(map[string]struct{})
	offer, err := searchAuthorizedReplacementOffer(ctx, exclude, search)
	if offer != nil {
		t.Fatalf("expected nil offer when all rejected, got %+v", offer)
	}
	if err == nil {
		t.Fatal("expected PriceAuthorizationRequiredError, got nil")
	}
	if !IsPriceAuthorizationRequired(err) {
		t.Fatalf("expected PriceAuthorizationRequiredError, got %T: %v", err, err)
	}
	var typed *PriceAuthorizationRequiredError
	if !errors.As(err, &typed) {
		t.Fatalf("errors.As failed: %v", err)
	}
	if typed.OfferedCents != 1100 {
		t.Errorf("OfferedCents = %d, want 1100", typed.OfferedCents)
	}
	if typed.MarketMedianCents != 1000 {
		t.Errorf("MarketMedianCents = %d, want 1000", typed.MarketMedianCents)
	}
	if calls < 1 {
		t.Errorf("inner search called %d times, want at least 1", calls)
	}
}

func TestPriceAuthorizationRequiredError_RendersWithAnchor(t *testing.T) {
	err := &PriceAuthorizationRequiredError{
		GPUClass:          "A100",
		GPUMemGB:          80,
		OfferedCents:      500,
		AnchorCents:       130,
		CeilingCents:      195,
		MarketMedianCents: 220,
		AnchorSamples:     4,
		AnchorWindowDays:  30,
	}
	msg := err.Error()
	for _, want := range []string{"requires price authorization", "$5.00/hr", "$1.95/hr", "A100", "≥80GB", "$1.30/hr", "4 samples", "30d"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
}

func TestPriceAuthorizationRequiredError_RendersWithoutAnchor(t *testing.T) {
	err := &PriceAuthorizationRequiredError{
		GPUClass:          "H200",
		GPUMemGB:          141,
		OfferedCents:      999,
		MarketMedianCents: 800,
	}
	msg := err.Error()
	for _, want := range []string{"no history yet", "H200", "≥141GB", "$9.99/hr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
}
