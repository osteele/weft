package dataloc

import (
	"strings"
	"testing"
)

func TestBuildHFDownloadCommand_Model(t *testing.T) {
	cmd, err := buildHFDownloadCommand(DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"}, "main")
	if err != nil {
		t.Fatalf("buildHFDownloadCommand: %v", err)
	}
	if !strings.Contains(cmd, "_hfdl=hf_xet") {
		t.Fatalf("command missing hf_xet candidate: %s", cmd)
	}
	if !strings.Contains(cmd, "_hfdl=hf") {
		t.Fatalf("command missing hf candidate: %s", cmd)
	}
	if !strings.Contains(cmd, "$_hfdl download --repo-type model") {
		t.Fatalf("command missing hf cli dispatch: %s", cmd)
	}
	if !strings.Contains(cmd, "snapshot_download") {
		t.Fatalf("command missing python fallback: %s", cmd)
	}
	if !strings.Contains(cmd, "meta-llama/Llama-3-8B") {
		t.Fatalf("command missing repo id: %s", cmd)
	}
}

func TestBuildHFDownloadCommand_Dataset(t *testing.T) {
	cmd, err := buildHFDownloadCommand(DataAsset{Kind: AssetHFDataset, ID: "HuggingFaceFW/fineweb"}, "refs/pr/1")
	if err != nil {
		t.Fatalf("buildHFDownloadCommand: %v", err)
	}
	if !strings.Contains(cmd, "--repo-type dataset") {
		t.Fatalf("command missing dataset repo type: %s", cmd)
	}
	if !strings.Contains(cmd, "refs/pr/1") {
		t.Fatalf("command missing revision: %s", cmd)
	}
}

func TestBuildHFDownloadCommand_RejectsUnsupportedKinds(t *testing.T) {
	if _, err := buildHFDownloadCommand(DataAsset{Kind: AssetCheckpoint, ID: "ckpt"}, "main"); err == nil {
		t.Fatal("expected unsupported asset kind to fail")
	}
}
