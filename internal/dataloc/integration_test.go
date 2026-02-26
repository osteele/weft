package dataloc

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/ssh"
	_ "modernc.org/sqlite"
)

func getTestHost(t *testing.T) string {
	host := os.Getenv("SSH_TEST_HOST")
	if host == "" {
		t.Skip("SSH_TEST_HOST not set - skipping integration test")
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("SSH_AUTH_SOCK not set - skipping integration test")
	}
	return host
}

func TestIntegrationScanHFCache(t *testing.T) {
	host := getTestHost(t)

	// Create fake HF cache directories on the remote host
	setupCmd := `mkdir -p ~/.cache/huggingface/hub/models--test-org--test-model ~/.cache/huggingface/hub/datasets--test-org--test-dataset`
	_, _, err := ssh.Run(host, setupCmd)
	if err != nil {
		t.Fatalf("setup HF cache dirs: %v", err)
	}
	t.Cleanup(func() {
		cleanupCmd := `rm -rf ~/.cache/huggingface/hub/models--test-org--test-model ~/.cache/huggingface/hub/datasets--test-org--test-dataset`
		ssh.Run(host, cleanupCmd)
	})

	assets, err := ScanHFCache(host)
	if err != nil {
		t.Fatalf("ScanHFCache: %v", err)
	}

	// Should find at least our two test entries (may find more if cache already has entries)
	foundModel := false
	foundDataset := false
	for _, a := range assets {
		if a.Kind == AssetHFModel && a.ID == "test-org/test-model" {
			foundModel = true
		}
		if a.Kind == AssetHFDataset && a.ID == "test-org/test-dataset" {
			foundDataset = true
		}
	}
	if !foundModel {
		t.Errorf("expected to find model test-org/test-model in %v", assets)
	}
	if !foundDataset {
		t.Errorf("expected to find dataset test-org/test-dataset in %v", assets)
	}
}

func TestIntegrationScanAndStore(t *testing.T) {
	host := getTestHost(t)

	// Create fake HF cache
	setupCmd := `mkdir -p ~/.cache/huggingface/hub/models--integration--scan-store-test`
	_, _, err := ssh.Run(host, setupCmd)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() {
		ssh.Run(host, `rm -rf ~/.cache/huggingface/hub/models--integration--scan-store-test`)
	})

	// Set up in-memory database
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := InitSchema(database); err != nil {
		t.Fatal(err)
	}

	// Scan and store
	assets, err := ScanHFCache(host)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	now := time.Now()
	for _, asset := range assets {
		if err := RecordAsset(database, HostDataEntry{
			Host:     host,
			Asset:    asset,
			LastSeen: now,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// Verify stored
	entries, err := ListHostAssets(database, host)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	found := false
	for _, e := range entries {
		if e.Asset.Kind == AssetHFModel && e.Asset.ID == "integration/scan-store-test" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected to find integration/scan-store-test in stored entries")
	}
}
