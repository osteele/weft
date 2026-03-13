package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
)

func TestRunDataFetchRecordsCompletedRequest(t *testing.T) {
	database := db.SetupTestDB(t)

	hostsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostsDir, "cool30.yaml"), []byte(`
name: cool30
os: linux
arch: amd64
cpu_cores: 16
memory: 64 GiB
gpus:
  - name: RTX 3090
    class: rtx3090
    memory: 24 GiB
    indices: [0]
`), 0o644); err != nil {
		t.Fatalf("write host inventory: %v", err)
	}
	cleanupHosts := inventory.SetHostsDir(hostsDir)
	t.Cleanup(cleanupHosts)

	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "cool30" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		switch {
		case strings.Contains(command, "hf download --repo-type model"),
			strings.Contains(command, "snapshot_download"):
			return "", "", nil
		case strings.Contains(command, "du -sb ~/.cache/huggingface/hub/models--*"):
			return "2048\t/home/test/.cache/huggingface/hub/models--bert-base-uncased\n", "", nil
		default:
			return "", "", fmt.Errorf("unexpected command %q", command)
		}
	})
	t.Cleanup(cleanupSSH)

	dataFetchHost = "cool30"
	dataFetchRev = "main"
	dataJSON = false

	out := captureStdout(t, func() {
		if err := runDataFetch(nil, []string{"hf:bert-base-uncased"}); err != nil {
			t.Fatalf("runDataFetch: %v", err)
		}
	})
	if !strings.Contains(out, "Request 1 completed") {
		t.Fatalf("output = %q", out)
	}

	requests, err := dataloc.ListDataRequests(database, "cool30")
	if err != nil {
		t.Fatalf("ListDataRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(requests))
	}
	if requests[0].Status != dataloc.RequestCompleted {
		t.Fatalf("status = %s, want %s", requests[0].Status, dataloc.RequestCompleted)
	}

	entries, err := dataloc.FindAssetHosts(database, dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "bert-base-uncased"})
	if err != nil {
		t.Fatalf("FindAssetHosts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].SizeBytes != 2048 {
		t.Fatalf("size = %d, want 2048", entries[0].SizeBytes)
	}
}

func TestRunDataWherePrintsKnownHosts(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:      "cool100",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "meta-llama/Llama-3-8B"},
		Path:      "/cache/models--meta-llama--Llama-3-8B",
		SizeBytes: 4096,
		LastSeen:  now,
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	dataJSON = false
	out := captureStdout(t, func() {
		if err := runDataWhere(nil, []string{"hf:meta-llama/Llama-3-8B"}); err != nil {
			t.Fatalf("runDataWhere: %v", err)
		}
	})
	if !strings.Contains(out, "cool100") {
		t.Fatalf("output missing host: %q", out)
	}
	if !strings.Contains(out, "HOST") || !strings.Contains(out, "PATH") {
		t.Fatalf("output missing table headers: %q", out)
	}
}

func TestParseDataAssetArgRejectsCheckpoint(t *testing.T) {
	if _, err := parseDataAssetArg("checkpoint:model"); err == nil {
		t.Fatal("expected checkpoint asset to be rejected")
	}
}
