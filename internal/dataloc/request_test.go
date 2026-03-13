package dataloc

import (
	"testing"
)

func TestCreateDataRequestAndList(t *testing.T) {
	db := setupTestDB(t)

	req, err := CreateDataRequest(db, CreateRequestParams{
		Host:     "host-alpha",
		Asset:    DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"},
		Revision: "main",
	})
	if err != nil {
		t.Fatalf("CreateDataRequest: %v", err)
	}
	if req.Status != RequestPending {
		t.Fatalf("status = %s, want %s", req.Status, RequestPending)
	}

	requests, err := ListDataRequests(db, "host-alpha")
	if err != nil {
		t.Fatalf("ListDataRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(requests))
	}
	if requests[0].Asset.ID != "meta-llama/Llama-3-8B" {
		t.Fatalf("asset ID = %q", requests[0].Asset.ID)
	}
}

func TestDataRequestLifecycle(t *testing.T) {
	db := setupTestDB(t)

	req, err := CreateDataRequest(db, CreateRequestParams{
		Host:  "host-beta",
		Asset: DataAsset{Kind: AssetHFDataset, ID: "HuggingFaceFW/fineweb"},
	})
	if err != nil {
		t.Fatalf("CreateDataRequest: %v", err)
	}

	if err := MarkDataRequestRunning(db, req.ID); err != nil {
		t.Fatalf("MarkDataRequestRunning: %v", err)
	}
	entry := HostDataEntry{
		Host:      "host-beta",
		Asset:     DataAsset{Kind: AssetHFDataset, ID: "HuggingFaceFW/fineweb"},
		Path:      "/home/test/.cache/huggingface/hub/datasets--HuggingFaceFW--fineweb",
		SizeBytes: 1234,
	}
	if err := MarkDataRequestCompleted(db, req.ID, entry); err != nil {
		t.Fatalf("MarkDataRequestCompleted: %v", err)
	}

	got, err := GetDataRequest(db, req.ID)
	if err != nil {
		t.Fatalf("GetDataRequest: %v", err)
	}
	if got.Status != RequestCompleted {
		t.Fatalf("status = %s, want %s", got.Status, RequestCompleted)
	}
	if got.RemotePath != entry.Path {
		t.Fatalf("remote_path = %q, want %q", got.RemotePath, entry.Path)
	}
	if got.SizeBytes != entry.SizeBytes {
		t.Fatalf("size_bytes = %d, want %d", got.SizeBytes, entry.SizeBytes)
	}
	if got.StartedAt == nil || got.CompletedAt == nil {
		t.Fatalf("expected started_at and completed_at to be populated")
	}
}

func TestMarkDataRequestFailed(t *testing.T) {
	db := setupTestDB(t)

	req, err := CreateDataRequest(db, CreateRequestParams{
		Host:  "host-gamma",
		Asset: DataAsset{Kind: AssetHFModel, ID: "google/gemma-2b"},
	})
	if err != nil {
		t.Fatalf("CreateDataRequest: %v", err)
	}

	if err := MarkDataRequestFailed(db, req.ID, "missing credentials"); err != nil {
		t.Fatalf("MarkDataRequestFailed: %v", err)
	}

	got, err := GetDataRequest(db, req.ID)
	if err != nil {
		t.Fatalf("GetDataRequest: %v", err)
	}
	if got.Status != RequestFailed {
		t.Fatalf("status = %s, want %s", got.Status, RequestFailed)
	}
	if got.Error != "missing credentials" {
		t.Fatalf("error = %q", got.Error)
	}
	if got.CompletedAt == nil {
		t.Fatalf("completed_at should be populated")
	}
}
