package db

import (
	"errors"
	"testing"
)

func TestUpsertAndGetNamedAsset(t *testing.T) {
	database := setupTestDB(t)

	a := NamedAsset{
		Name:        "exp207-eval-llama8b",
		ContentHash: "deadbeef",
		SizeBytes:   1024,
		TargetPath:  "output/exp207_eval_texts.pkl",
	}
	if err := UpsertNamedAsset(database, a); err != nil {
		t.Fatalf("UpsertNamedAsset: %v", err)
	}

	got, err := GetNamedAssetByName(database, a.Name)
	if err != nil {
		t.Fatalf("GetNamedAssetByName: %v", err)
	}
	if got.ContentHash != a.ContentHash {
		t.Errorf("ContentHash = %q, want %q", got.ContentHash, a.ContentHash)
	}
	if got.TargetPath != a.TargetPath {
		t.Errorf("TargetPath = %q, want %q", got.TargetPath, a.TargetPath)
	}
	if got.SizeBytes != a.SizeBytes {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, a.SizeBytes)
	}

	// Republish overwrites content_hash + target_path.
	a.ContentHash = "feedface"
	a.TargetPath = "output/v2.pkl"
	a.SizeBytes = 2048
	if err := UpsertNamedAsset(database, a); err != nil {
		t.Fatalf("UpsertNamedAsset (overwrite): %v", err)
	}
	got, err = GetNamedAssetByName(database, a.Name)
	if err != nil {
		t.Fatalf("GetNamedAssetByName after overwrite: %v", err)
	}
	if got.ContentHash != "feedface" || got.TargetPath != "output/v2.pkl" {
		t.Errorf("overwrite did not take effect: got %+v", got)
	}
}

func TestGetNamedAsset_NotFound(t *testing.T) {
	database := setupTestDB(t)
	_, err := GetNamedAssetByName(database, "no-such-asset")
	if !errors.Is(err, ErrNamedAssetNotFound) {
		t.Fatalf("expected ErrNamedAssetNotFound, got %v", err)
	}
}

func TestUpsertNamedAsset_Validation(t *testing.T) {
	database := setupTestDB(t)
	cases := []struct {
		name string
		a    NamedAsset
	}{
		{"empty name", NamedAsset{ContentHash: "x", TargetPath: "y"}},
		{"empty hash", NamedAsset{Name: "n", TargetPath: "y"}},
		{"empty target", NamedAsset{Name: "n", ContentHash: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := UpsertNamedAsset(database, tc.a); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}
