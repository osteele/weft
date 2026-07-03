package campaign

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

func TestResolveTransportableCheckpointNeedsMirrorsInputToCloudNeed(t *testing.T) {
	database := db.SetupTestDB(t)
	hash := strings.Repeat("b", 64)
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:        "host-beta",
		Asset:       dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "trace"},
		Path:        "/mnt/checkpoints/trace",
		SizeBytes:   4096,
		ContentHash: hash,
		ContentType: dataloc.ContentTypeDirectory,
		LastSeen:    time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	if err := db.UpsertNamedAsset(database, db.NamedAsset{
		Name:        "trace-v1",
		ContentHash: hash,
		SizeBytes:   4096,
		ContentType: string(dataloc.ContentTypeDirectory),
		TargetPath:  "data/trace",
	}); err != nil {
		t.Fatalf("UpsertNamedAsset: %v", err)
	}

	prev := checkpointObjectExistsFunc
	t.Cleanup(func() { checkpointObjectExistsFunc = prev })
	checkpointObjectExistsFunc = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return true, nil
	}

	needs, err := resolveTransportableCheckpointNeeds(context.Background(), database, &r2.Client{}, &db.Job{
		ID:     44,
		Inputs: []string{"checkpoint:trace"},
	})
	if err != nil {
		t.Fatalf("resolveTransportableCheckpointNeeds: %v", err)
	}
	if len(needs) != 1 {
		t.Fatalf("needs = %v, want one", needs)
	}
	if needs[0].Spec != "checkpoint:trace" {
		t.Fatalf("spec = %q", needs[0].Spec)
	}
	if needs[0].Path != "data/trace" {
		t.Fatalf("path = %q, want named asset target path", needs[0].Path)
	}
	if needs[0].ContentType != string(dataloc.ContentTypeDirectory) {
		t.Fatalf("content type = %q, want directory", needs[0].ContentType)
	}
}

func TestPublishCheckpointToR2SkipsWhenObjectAlreadyExists(t *testing.T) {
	database := db.SetupTestDB(t)
	hash := strings.Repeat("e", 64)
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:        "host-beta",
		Asset:       dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "trace"},
		Path:        "/mnt/checkpoints/trace",
		SizeBytes:   4096,
		ContentHash: hash,
		ContentType: dataloc.ContentTypeDirectory,
		LastSeen:    time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	prevExists := checkpointObjectExistsFunc
	prevDigest := checkpointDigestFromHostFunc
	prevPut := checkpointR2PutFromHostFunc
	t.Cleanup(func() {
		checkpointObjectExistsFunc = prevExists
		checkpointDigestFromHostFunc = prevDigest
		checkpointR2PutFromHostFunc = prevPut
	})
	checkpointObjectExistsFunc = func(context.Context, *r2.Client, string) (bool, error) {
		return true, nil
	}
	checkpointDigestFromHostFunc = func(context.Context, string, string) (dataloc.ContentInfo, error) {
		t.Fatal("digest should not run for an already-resident object")
		return dataloc.ContentInfo{}, nil
	}
	checkpointR2PutFromHostFunc = func(context.Context, string, dataloc.R2PutFromHostRequest) error {
		t.Fatal("put should not run for an already-resident object")
		return nil
	}

	result, err := PublishCheckpointToR2(context.Background(), database, &r2.Client{}, cloud.R2Config{}, dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "trace"})
	if err != nil {
		t.Fatalf("PublishCheckpointToR2: %v", err)
	}
	if !result.AlreadyResident || result.Uploaded {
		t.Fatalf("result = %+v, want already resident without upload", result)
	}
}

func TestPublishCheckpointToR2ConfirmsHolderThenUploads(t *testing.T) {
	database := db.SetupTestDB(t)
	hash := strings.Repeat("f", 64)
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:        "host-beta",
		Asset:       dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "trace"},
		Path:        "/mnt/checkpoints/trace",
		SizeBytes:   4096,
		ContentHash: hash,
		ContentType: dataloc.ContentTypeDirectory,
		LastSeen:    time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	prevExists := checkpointObjectExistsFunc
	prevDigest := checkpointDigestFromHostFunc
	prevPut := checkpointR2PutFromHostFunc
	t.Cleanup(func() {
		checkpointObjectExistsFunc = prevExists
		checkpointDigestFromHostFunc = prevDigest
		checkpointR2PutFromHostFunc = prevPut
	})
	checkpointObjectExistsFunc = func(context.Context, *r2.Client, string) (bool, error) {
		return false, nil
	}
	checkpointDigestFromHostFunc = func(_ context.Context, host, path string) (dataloc.ContentInfo, error) {
		if host != "host-beta" || path != "/mnt/checkpoints/trace" {
			t.Fatalf("digest = %s:%s", host, path)
		}
		return dataloc.ContentInfo{Hash: hash, SizeBytes: 4096, ContentType: dataloc.ContentTypeDirectory}, nil
	}
	putCalls := 0
	checkpointR2PutFromHostFunc = func(_ context.Context, host string, req dataloc.R2PutFromHostRequest) error {
		putCalls++
		if host != "host-beta" || req.Path != "/mnt/checkpoints/trace" {
			t.Fatalf("put = %s:%s", host, req.Path)
		}
		if req.Key != "assets/"+hash {
			t.Fatalf("key = %s", req.Key)
		}
		return nil
	}

	result, err := PublishCheckpointToR2(context.Background(), database, &r2.Client{}, cloud.R2Config{}, dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "trace"})
	if err != nil {
		t.Fatalf("PublishCheckpointToR2: %v", err)
	}
	if !result.Uploaded || putCalls != 1 {
		t.Fatalf("uploaded=%v putCalls=%d, want true/1", result.Uploaded, putCalls)
	}
}
