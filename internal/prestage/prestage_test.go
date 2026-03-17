package prestage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := dataloc.InitSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestBuildPlan_MissingData(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Place model on cool30 with a known path
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:      "host-beta",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "meta-llama/Llama-3-8B"},
		Path:      "/data/models/meta-llama/Llama-3-8B",
		SizeBytes: 16_000_000_000,
		LastSeen:  now,
	}); err != nil {
		t.Fatal(err)
	}

	// Build plan targeting cool100 (doesn't have the model)
	plan, err := BuildPlan(db, "host-alpha", "/home/test/.cache/huggingface/hub", []string{"hf:meta-llama/Llama-3-8B"})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(plan.Transfers))
	}

	tr := plan.Transfers[0]
	if tr.SourceHost != "host-beta" {
		t.Errorf("source host: got %s, want cool30", tr.SourceHost)
	}
	if tr.SizeBytes != 16_000_000_000 {
		t.Errorf("size: got %d, want 16000000000", tr.SizeBytes)
	}
	if tr.RemotePath != "/data/models/meta-llama/Llama-3-8B" {
		t.Errorf("path: got %s, want /data/models/meta-llama/Llama-3-8B", tr.RemotePath)
	}
	if tr.Asset.ID != "meta-llama/Llama-3-8B" {
		t.Errorf("asset ID: got %s, want meta-llama/Llama-3-8B", tr.Asset.ID)
	}
}

func TestBuildPlan_AllLocal(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Place model on cool100 (the target)
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:      "host-alpha",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "model-a"},
		Path:      "/data/models/model-a",
		SizeBytes: 5_000_000_000,
		LastSeen:  now,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(db, "host-alpha", "", []string{"hf:model-a"})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Transfers) != 0 {
		t.Errorf("expected 0 transfers when data is local, got %d", len(plan.Transfers))
	}
}

func TestBuildPlan_MultipleInputs(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// model-a on cool30
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:      "host-beta",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "model-a"},
		Path:      "/data/models/model-a",
		SizeBytes: 10_000_000_000,
		LastSeen:  now,
	}); err != nil {
		t.Fatal(err)
	}

	// dataset-b on cool100 (the target) — should not need transfer
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:      "host-alpha",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFDataset, ID: "wikitext"},
		Path:      "/data/datasets/wikitext",
		SizeBytes: 1_000_000_000,
		LastSeen:  now,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(db, "host-alpha", "/home/test/.cache/huggingface/hub", []string{"hf:model-a", "hf-dataset:wikitext"})
	if err != nil {
		t.Fatal(err)
	}

	// Only model-a needs transfer
	if len(plan.Transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(plan.Transfers))
	}
	if plan.Transfers[0].Asset.ID != "model-a" {
		t.Errorf("transfer asset: got %s, want model-a", plan.Transfers[0].Asset.ID)
	}
}

func TestBuildPlan_NoSourceWithPath(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Asset exists on cool30 but without a path — can't rsync
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:      "host-beta",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "model-x"},
		Path:      "",
		SizeBytes: 5_000_000_000,
		LastSeen:  now,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(db, "host-alpha", "", []string{"hf:model-x"})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Transfers) != 0 {
		t.Errorf("expected 0 transfers when no source has a path, got %d", len(plan.Transfers))
	}
}

func TestBuildPlan_UnknownAsset(t *testing.T) {
	db := setupTestDB(t)

	// No assets recorded at all
	plan, err := BuildPlan(db, "host-alpha", "", []string{"hf:unknown-model"})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Transfers) != 0 {
		t.Errorf("expected 0 transfers for unknown asset, got %d", len(plan.Transfers))
	}
}

func TestPlan_TotalBytes(t *testing.T) {
	plan := &Plan{
		Host: "host-alpha",
		Transfers: []Transfer{
			{SizeBytes: 10_000_000_000},
			{SizeBytes: 5_000_000_000},
		},
	}

	if got := plan.TotalBytes(); got != 15_000_000_000 {
		t.Errorf("TotalBytes: got %d, want 15000000000", got)
	}
}

func TestPlan_TotalBytes_Empty(t *testing.T) {
	plan := &Plan{Host: "host-alpha"}
	if got := plan.TotalBytes(); got != 0 {
		t.Errorf("TotalBytes: got %d, want 0", got)
	}
}
