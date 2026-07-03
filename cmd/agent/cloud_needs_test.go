package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/runner"
)

func TestStageCloudNeeds_Success(t *testing.T) {
	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, ".home")
	t.Setenv("HOME", homeDir)
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
	marker := runner.ArtifactSatisfiedFile(filepath.Join(homeDir, ".cache", "weft", "logs"), "output/model.pt", 41)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected satisfied marker %s: %v", marker, err)
	}
}

func TestStageCloudNeeds_NamedAssetWritesMarker(t *testing.T) {
	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, ".home")
	t.Setenv("HOME", homeDir)
	needs := []cloud.CloudNeed{{
		Spec:  "asset:trace-v1",
		Path:  "data/trace.jsonl",
		R2Key: "assets/sha256/abc",
	}}

	prev := copyCloudNeedFromR2Func
	t.Cleanup(func() { copyCloudNeedFromR2Func = prev })
	copyCloudNeedFromR2Func = func(_, _, targetPath string) error {
		return os.WriteFile(targetPath, []byte("ok"), 0o644)
	}

	if err := stageCloudNeeds("bucket", 123, workDir, needs); err != nil {
		t.Fatalf("stageCloudNeeds: %v", err)
	}
	marker := runner.NamedAssetSatisfiedFile(filepath.Join(homeDir, ".cache", "weft", "logs"), "trace-v1")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(data) != "0\n" {
		t.Fatalf("marker = %q, want 0", string(data))
	}
}

func TestStageCloudNeeds_DirectoryAssetExtractsArchive(t *testing.T) {
	workDir := t.TempDir()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "weights.bin"), []byte("weights"), 0o644); err != nil {
		t.Fatalf("write weights: %v", err)
	}
	if err := os.Mkdir(filepath.Join(srcDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	needs := []cloud.CloudNeed{{
		Spec:        "checkpoint:trace",
		Path:        "checkpoints/trace",
		R2Key:       "assets/hash",
		ContentType: string(dataloc.ContentTypeDirectory),
	}}

	prev := copyCloudNeedFromR2Func
	t.Cleanup(func() { copyCloudNeedFromR2Func = prev })
	copyCloudNeedFromR2Func = func(_, _, targetPath string) error {
		f, err := os.Create(targetPath)
		if err != nil {
			return err
		}
		err = dataloc.WriteDirectoryArchive(srcDir, f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	}

	if err := stageCloudNeeds("bucket", 123, workDir, needs); err != nil {
		t.Fatalf("stageCloudNeeds: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "checkpoints", "trace", "nested", "config.json"))
	if err != nil {
		t.Fatalf("read extracted config: %v", err)
	}
	if string(data) != "{}" {
		t.Fatalf("config = %q", data)
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
