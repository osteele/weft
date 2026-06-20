package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
)

func TestCollectPriceAuthorizationBlocks(t *testing.T) {
	database := db.SetupTestDB(t)

	priceJob, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "expensive launch", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU priceJob: %v", err)
	}
	mem := 80
	if err := db.SetJobGPUClass(database, priceJob, "A100"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	if err := db.SetJobGPUMemGB(database, priceJob, &mem); err != nil {
		t.Fatalf("SetJobGPUMemGB: %v", err)
	}
	reason := "requires price authorization: offered $5.00/hr exceeds your authorized $2.00/hr for A100 >=80GB"
	if err := db.SetJobPlacementBlocked(database, priceJob, (&blockreason.Structured{
		Summary: reason,
		Launch:  reason,
	}).Marshal()); err != nil {
		t.Fatalf("SetJobPlacementBlocked priceJob: %v", err)
	}

	otherJob, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python other.py", "ordinary block", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU otherJob: %v", err)
	}
	if err := db.SetJobPlacementBlocked(database, otherJob, (&blockreason.Structured{
		Summary: "no offers available",
		Launch:  "no offers available",
	}).Marshal()); err != nil {
		t.Fatalf("SetJobPlacementBlocked otherJob: %v", err)
	}

	placedJob, err := db.RecordQueuedWithGPU(database, "cool30", t.TempDir(), "python placed.py", "placed", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU placedJob: %v", err)
	}
	if err := db.SetJobPlacementBlocked(database, placedJob, (&blockreason.Structured{
		Summary: reason,
		Launch:  reason,
	}).Marshal()); err != nil {
		t.Fatalf("SetJobPlacementBlocked placedJob: %v", err)
	}

	blocks, err := collectPriceAuthorizationBlocks(database)
	if err != nil {
		t.Fatalf("collectPriceAuthorizationBlocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %+v, want exactly one price-auth block", blocks)
	}
	got := blocks[0]
	if got.JobID != "wj1" || got.GPUClass != "A100" || got.GPUMemGB != 80 || got.GPUBucket != "A100 >=80GB" {
		t.Fatalf("block = %+v, want wj1 A100 >=80GB", got)
	}
	if got.OfferedCents != 500 {
		t.Fatalf("OfferedCents = %d, want 500", got.OfferedCents)
	}
	if got.AuthorizeCommand != "weft job authorize-price wj1 --up-to 5.00" {
		t.Fatalf("AuthorizeCommand = %q", got.AuthorizeCommand)
	}
}

func TestFormatAutopilotStatusTextShowsPriceAuthorizationBlocks(t *testing.T) {
	view := autopilotStateView{
		State: stateIdle,
		PriceAuthBlocks: []priceAuthBlockView{{
			JobID:            "wj42",
			Description:      "calibration",
			GPUBucket:        "H100 >=80GB",
			Reason:           "requires price authorization: offered $9.00/hr",
			AuthorizeCommand: "weft job authorize-price wj42 --up-to 9.00",
		}},
	}
	out := formatAutopilotStatusText(view)
	for _, want := range []string{
		"blocked_on_price_authorization: 1 job",
		"wj42 H100 >=80GB",
		"reason: requires price authorization",
		"authorize: weft job authorize-price wj42 --up-to 9.00",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
}

func TestAutopilotStateViewJSONIncludesPriceAuthorizationBlocks(t *testing.T) {
	view := autopilotStateView{
		State: stateIdle,
		PriceAuthBlocks: []priceAuthBlockView{{
			JobID:            "wj7",
			GPUBucket:        "A100 >=80GB",
			Reason:           "requires price authorization",
			OfferedCents:     500,
			AuthorizeCommand: "weft job authorize-price wj7 --up-to 5.00",
		}},
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"blocked_on_price_authorization"`, `"job_id":"wj7"`, `"offered_cents":500`} {
		if !strings.Contains(s, want) {
			t.Fatalf("json missing %s: %s", want, s)
		}
	}
}
