package dataloc

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := InitSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestInitSchema(t *testing.T) {
	db := setupTestDB(t)

	// Should be idempotent
	if err := InitSchema(db); err != nil {
		t.Fatalf("second InitSchema: %v", err)
	}
}

func TestRecordAsset_Insert(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	entry := HostDataEntry{
		Host:      "cool100",
		Asset:     DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"},
		Path:      "~/.cache/huggingface/hub/models--meta-llama--Llama-3-8B",
		SizeBytes: 16_000_000_000,
		LastSeen:  now,
	}
	if err := RecordAsset(db, entry); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	entries, err := ListHostAssets(db, "cool100")
	if err != nil {
		t.Fatalf("ListHostAssets: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Asset.Kind != AssetHFModel || got.Asset.ID != "meta-llama/Llama-3-8B" {
		t.Errorf("asset mismatch: %v", got.Asset)
	}
	if got.SizeBytes != 16_000_000_000 {
		t.Errorf("size: got %d, want 16000000000", got.SizeBytes)
	}
	if !got.LastSeen.Equal(now) {
		t.Errorf("last_seen: got %v, want %v", got.LastSeen, now)
	}
}

func TestRecordAsset_Upsert(t *testing.T) {
	db := setupTestDB(t)
	t1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	t2 := time.Now().Truncate(time.Second)

	entry := HostDataEntry{
		Host:      "cool100",
		Asset:     DataAsset{Kind: AssetHFModel, ID: "bert-base"},
		SizeBytes: 1000,
		LastSeen:  t1,
	}
	if err := RecordAsset(db, entry); err != nil {
		t.Fatal(err)
	}

	// Update with new timestamp and size
	entry.SizeBytes = 2000
	entry.LastSeen = t2
	if err := RecordAsset(db, entry); err != nil {
		t.Fatal(err)
	}

	entries, err := ListHostAssets(db, "cool100")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("upsert should not duplicate: got %d entries", len(entries))
	}
	if entries[0].SizeBytes != 2000 {
		t.Errorf("size not updated: got %d", entries[0].SizeBytes)
	}
	if !entries[0].LastSeen.Equal(t2) {
		t.Errorf("last_seen not updated: got %v, want %v", entries[0].LastSeen, t2)
	}
}

func TestListHostAssets_Empty(t *testing.T) {
	db := setupTestDB(t)
	entries, err := ListHostAssets(db, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty, got %d", len(entries))
	}
}

func TestListHostAssets_MultipleHosts(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	assets := []HostDataEntry{
		{Host: "cool100", Asset: DataAsset{AssetHFModel, "llama"}, LastSeen: now},
		{Host: "cool100", Asset: DataAsset{AssetHFDataset, "wikitext"}, LastSeen: now},
		{Host: "cool30", Asset: DataAsset{AssetHFModel, "llama"}, LastSeen: now},
	}
	for _, a := range assets {
		if err := RecordAsset(db, a); err != nil {
			t.Fatal(err)
		}
	}

	cool100, err := ListHostAssets(db, "cool100")
	if err != nil {
		t.Fatal(err)
	}
	if len(cool100) != 2 {
		t.Errorf("cool100: got %d assets, want 2", len(cool100))
	}

	cool30, err := ListHostAssets(db, "cool30")
	if err != nil {
		t.Fatal(err)
	}
	if len(cool30) != 1 {
		t.Errorf("cool30: got %d assets, want 1", len(cool30))
	}
}

func TestFindAssetHosts(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	llama := DataAsset{AssetHFModel, "meta-llama/Llama-3-8B"}

	// Record on two hosts
	for _, host := range []string{"cool100", "cool30"} {
		if err := RecordAsset(db, HostDataEntry{
			Host: host, Asset: llama, LastSeen: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	hosts, err := FindAssetHosts(db, llama)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want 2", len(hosts))
	}

	// Should not find unrelated asset
	hosts, err = FindAssetHosts(db, DataAsset{AssetHFModel, "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Errorf("expected 0 hosts for nonexistent asset, got %d", len(hosts))
	}
}

func TestRemoveStaleEntries(t *testing.T) {
	db := setupTestDB(t)
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	recent := time.Now().Truncate(time.Second)

	if err := RecordAsset(db, HostDataEntry{
		Host: "cool100", Asset: DataAsset{AssetHFModel, "old-model"}, LastSeen: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := RecordAsset(db, HostDataEntry{
		Host: "cool100", Asset: DataAsset{AssetHFModel, "new-model"}, LastSeen: recent,
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	removed, err := RemoveStaleEntries(db, "cool100", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}

	entries, err := ListHostAssets(db, "cool100")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Asset.ID != "new-model" {
		t.Errorf("wrong entry survived: %s", entries[0].Asset.ID)
	}
}

func TestRemoveStaleEntries_OnlyAffectsSpecifiedHost(t *testing.T) {
	db := setupTestDB(t)
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	if err := RecordAsset(db, HostDataEntry{
		Host: "cool100", Asset: DataAsset{AssetHFModel, "model"}, LastSeen: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := RecordAsset(db, HostDataEntry{
		Host: "cool30", Asset: DataAsset{AssetHFModel, "model"}, LastSeen: old,
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now()
	removed, err := RemoveStaleEntries(db, "cool100", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}

	// cool30's entry should still exist
	entries, err := ListHostAssets(db, "cool30")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("cool30 entry should survive, got %d", len(entries))
	}
}

func TestListAllAssets(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Record assets on multiple hosts
	assets := []HostDataEntry{
		{Host: "cool30", Asset: DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"}, LastSeen: now},
		{Host: "cool100", Asset: DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"}, LastSeen: now},
		{Host: "cool100", Asset: DataAsset{Kind: AssetHFDataset, ID: "allenai/dolma"}, LastSeen: now},
	}
	for _, a := range assets {
		if err := RecordAsset(db, a); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	all, err := ListAllAssets(db)
	if err != nil {
		t.Fatalf("ListAllAssets: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(all))
	}

	// Ordered by kind, id, host — datasets come before models alphabetically
	if all[0].Asset.Kind != AssetHFDataset {
		t.Errorf("first entry should be dataset, got %s", all[0].Asset.Kind)
	}
	// The two model entries should be grouped together
	if all[1].Asset.ID != all[2].Asset.ID {
		t.Errorf("model entries should be grouped: got %s and %s", all[1].Asset.ID, all[2].Asset.ID)
	}
}
