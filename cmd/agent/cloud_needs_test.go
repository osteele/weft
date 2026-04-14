package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestStageCloudNeeds_Success(t *testing.T) {
	workDir := t.TempDir()
	needs := []cloud.CloudNeed{{
		Spec:  "output/model.pt:41",
		Path:  "output/model.pt",
		R2Key: "jobs/41/runs/2/artifacts/files/output/model.pt",
	}}

	prev := copyCloudNeedFromR2Func
	t.Cleanup(func() { copyCloudNeedFromR2Func = prev })
	copyCloudNeedFromR2Func = func(bucket, r2Key, targetPath string) error {
		if bucket != "bucket" {
			t.Fatalf("bucket = %q, want bucket", bucket)
		}
		if r2Key != needs[0].R2Key {
			t.Fatalf("r2Key = %q, want %q", r2Key, needs[0].R2Key)
		}
		return os.WriteFile(targetPath, []byte("ok"), 0o644)
	}

	if err := stageCloudNeeds("bucket", 123, workDir, needs); err != nil {
		t.Fatalf("stageCloudNeeds: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "output", "model.pt"))
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("staged content = %q, want ok", string(data))
	}
}

func TestStageCloudNeeds_Failure(t *testing.T) {
	workDir := t.TempDir()
	needs := []cloud.CloudNeed{{
		Spec:  "output/model.pt:41",
		Path:  "output/model.pt",
		R2Key: "jobs/41/runs/2/artifacts/files/output/model.pt",
	}}

	prev := copyCloudNeedFromR2Func
	t.Cleanup(func() { copyCloudNeedFromR2Func = prev })
	copyCloudNeedFromR2Func = func(_, _, _ string) error {
		return fmt.Errorf("download failed")
	}

	if err := stageCloudNeeds("bucket", 123, workDir, needs); err == nil {
		t.Fatal("expected staging error")
	}
}
