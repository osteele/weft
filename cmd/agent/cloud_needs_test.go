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

func TestStageCloudNeedsRemovesDirectoryArchiveBeforeNextNeed(t *testing.T) {
	workDir := t.TempDir()
	needs := []cloud.CloudNeed{
		{Spec: "checkpoint:first", Path: "checkpoints/first", R2Key: "assets/first", ContentType: string(dataloc.ContentTypeDirectory)},
		{Spec: "checkpoint:second", Path: "checkpoints/second", R2Key: "assets/second", ContentType: string(dataloc.ContentTypeDirectory)},
	}

	prev := copyCloudNeedFromR2Func
	t.Cleanup(func() { copyCloudNeedFromR2Func = prev })
	copyCount := 0
	copyCloudNeedFromR2Func = func(_, _, targetPath string) error {
		copyCount++
		if copyCount == 2 {
			firstArchive := filepath.Join(workDir, "checkpoints", "first") + ".weft-archive.tar.gz"
			if _, err := os.Stat(firstArchive); !os.IsNotExist(err) {
				t.Fatalf("first archive still present before second need: %v", err)
			}
		}
		f, err := os.Create(targetPath)
		if err != nil {
			return err
		}
		sourceDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(sourceDir, "data.bin"), []byte("data"), 0o644); err != nil {
			return err
		}
		err = dataloc.WriteDirectoryArchive(sourceDir, f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	}

	if err := stageCloudNeeds("bucket", 123, workDir, needs); err != nil {
		t.Fatalf("stageCloudNeeds: %v", err)
	}
}

func TestStageCloudNeeds_ProducerDirectoryMarksOnlyAfterAllChildren(t *testing.T) {
	for _, failLast := range []bool{false, true} {
		t.Run(fmt.Sprintf("last-child-fails=%t", failLast), func(t *testing.T) {
			workDir := t.TempDir()
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)
			const spec = "output/model:41"
			needs := []cloud.CloudNeed{
				{Spec: spec, Path: "output/model/weights.bin", R2Key: "jobs/41/runs/7/outputs/output/model/weights.bin"},
				{Spec: spec, Path: "output/model/nested/config.json", R2Key: "jobs/41/runs/7/outputs/output/model/nested/config.json"},
			}
			marker := runner.ArtifactSatisfiedFile(filepath.Join(homeDir, ".cache", "weft", "logs"), "output/model", 41)
			prev := copyCloudNeedFromR2Func
			t.Cleanup(func() { copyCloudNeedFromR2Func = prev })
			copyCloudNeedFromR2Func = func(_, key, targetPath string) error {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("directory marked ready before all children staged: %v", err)
				}
				if failLast && key == needs[1].R2Key {
					return fmt.Errorf("interrupted nested download")
				}
				return os.WriteFile(targetPath, []byte(key), 0o644)
			}
			err := stageCloudNeeds("bucket", 123, workDir, needs)
			if failLast {
				if err == nil {
					t.Fatal("partial directory accepted")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("partial directory acquired satisfied marker: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, need := range needs {
				got, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(need.Path)))
				if err != nil || string(got) != need.R2Key {
					t.Fatalf("child %s = %q, %v", need.Path, got, err)
				}
			}
			if got, err := os.ReadFile(marker); err != nil || string(got) != "0\n" {
				t.Fatalf("directory satisfied marker = %q, %v", got, err)
			}
		})
	}
}

func TestStageCloudNeeds_RejectsEscapingDestination(t *testing.T) {
	for _, tc := range []struct {
		destination, contentType string
	}{
		{destination: "../escape"},
		{destination: "/../escape"},
		{destination: `output\escape`},
		{destination: "output/link/escape"},
		{destination: "output/root", contentType: "directory"},
	} {
		t.Run(tc.destination, func(t *testing.T) {
			workDir := t.TempDir()
			outside := t.TempDir()
			if err := os.Mkdir(filepath.Join(workDir, "output"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(workDir, "output", "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("..", filepath.Join(workDir, "output", "root")); err != nil {
				t.Fatal(err)
			}
			prev := copyCloudNeedFromR2Func
			t.Cleanup(func() { copyCloudNeedFromR2Func = prev })
			copyCloudNeedFromR2Func = func(_, _, target string) error {
				t.Fatalf("unsafe destination reached downloader: %s", target)
				return nil
			}
			err := stageCloudNeeds("bucket", 123, workDir, []cloud.CloudNeed{
				{Spec: "output/model:41", Path: tc.destination, R2Key: "jobs/41/runs/7/outputs/output/model/file", ContentType: tc.contentType},
			})
			if err == nil {
				t.Fatal("escaping destination accepted")
			}
		})
	}
}

func TestStageCloudNeeds_PreservesWorkdirPaths(t *testing.T) {
	for _, tc := range []struct {
		name, path, destination, link, linkTarget string
		directoryLink                             bool
	}{
		{name: "in-tree directory symlink", path: "models/nested/model.pt", destination: "data/nested/model.pt", link: "models", linkTarget: "data", directoryLink: true},
		{name: "in-tree file symlink", path: "model.pt", destination: "data/model.pt", link: "model.pt", linkTarget: "data/model.pt"},
		{name: "workdir alias parent", path: "root/model.pt", destination: "model.pt", link: "root", linkTarget: ".", directoryLink: true},
		{name: "leading slash", path: "/data/model.pt", destination: "data/model.pt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			if tc.link != "" {
				linkTarget := filepath.Join(workDir, tc.linkTarget)
				if tc.directoryLink {
					if err := os.MkdirAll(linkTarget, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.MkdirAll(filepath.Dir(linkTarget), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(linkTarget, []byte("old"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(tc.linkTarget, filepath.Join(workDir, tc.link)); err != nil {
					t.Fatal(err)
				}
			}
			previousCopy := copyCloudNeedFromR2Func
			t.Cleanup(func() { copyCloudNeedFromR2Func = previousCopy })
			copyCloudNeedFromR2Func = func(_, _, target string) error {
				return os.WriteFile(target, []byte("model"), 0o644)
			}
			needs := []cloud.CloudNeed{{Spec: tc.path + ":41", Path: tc.path, R2Key: "jobs/41/runs/7/outputs/data/model.pt"}}
			if err := stageCloudNeeds("bucket", 123, workDir, needs); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(workDir, tc.destination))
			if err != nil || string(got) != "model" {
				t.Fatalf("staged model = %q, %v", got, err)
			}
		})
	}
}
