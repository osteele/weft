package campaign

import (
	"context"
	"path"
	"testing"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	weftsync "github.com/osteele/weft/internal/sync"
)

func pinnedSourceTestJob(id int64, workingDir, key string) *db.Job {
	return &db.Job{
		ID:         id,
		WorkingDir: workingDir,
		Command:    "python train.py",
		Metadata: &db.JobMetadata{Source: &db.JobSourceMetadata{Pin: &db.JobSourcePinMetadata{
			Hash: "manifest-" + key,
			Roots: []db.JobSourcePinRootMetadata{{
				LocalPath:     workingDir,
				MountBasename: path.Base(workingDir),
				MountRel:      ".",
				Hash:          "hash-" + key,
				R2Key:         key,
				SizeBytes:     123,
				Blobs: []dataplane.SourceBlob{{
					R2Key:   "assets/blob",
					RelPath: "weights.bin",
					SHA256:  "blob-sha",
				}},
			}},
		}}},
	}
}

func TestFreshLaunchSourcePlanUsesPinnedManifestAndLegacyFallback(t *testing.T) {
	pinned := pinnedSourceTestJob(1, "/projects/shared", "sources/pinned.tar.gz")
	legacy := &db.Job{ID: 2, WorkingDir: "/projects/legacy", Command: "python legacy.py", Inputs: []string{"hf:gpt2"}}

	uploads, pinnedByJob, err := planSourceStaging([]InstanceGroup{{Jobs: []*db.Job{pinned, legacy}}})
	if err != nil {
		t.Fatalf("planSourceStaging: %v", err)
	}
	if _, ok := uploads[pinned.WorkingDir]; ok {
		t.Fatalf("pinned directory %q was scheduled for dispatch-time derivation", pinned.WorkingDir)
	}
	if spec, ok := uploads[legacy.WorkingDir]; !ok || len(spec.commands) != 1 || spec.commands[0] != legacy.Command {
		t.Fatalf("legacy fallback upload = %#v, want command %q", spec, legacy.Command)
	}
	manifest, ok := pinnedByJob[pinned.ID]
	if !ok || manifest.Roots[0].R2Key != "sources/pinned.tar.gz" {
		t.Fatalf("pinned manifest = %#v", manifest)
	}

	localToRemote := map[string]string{
		pinned.WorkingDir: "/workspace/shared",
		legacy.WorkingDir: "/workspace/legacy",
	}
	legacyManifest := weftsync.SourceManifest{Roots: []weftsync.SourceRoot{{
		LocalPath: legacy.WorkingDir,
		R2Key:     "sources/current-tree.tar.gz",
	}}}
	sources := sourceMappingsForLaunch(
		InstanceGroup{Jobs: []*db.Job{pinned, legacy}},
		localToRemote,
		pinnedByJob,
		map[string]weftsync.SourceManifest{legacy.WorkingDir: legacyManifest},
		nil,
	)
	if len(sources) != 2 {
		t.Fatalf("source mappings = %#v, want pinned and fallback mappings", sources)
	}
	if sources[0].R2Key != "sources/pinned.tar.gz" {
		t.Fatalf("pinned launch key = %q, want stored key", sources[0].R2Key)
	}
	if sources[1].R2Key != "sources/current-tree.tar.gz" {
		t.Fatalf("legacy launch key = %q, want dispatch-derived fallback", sources[1].R2Key)
	}
}

func TestReuseSourceUploadUsesPinnedManifestAndLegacyFallback(t *testing.T) {
	originalUpload := uploadSourceRootsToR2
	t.Cleanup(func() { uploadSourceRootsToR2 = originalUpload })

	var uploads []string
	uploadSourceRootsToR2 = func(_ context.Context, _ *r2.Client, localDir string, _ []string, _ []string) (weftsync.SourceUploadResult, error) {
		uploads = append(uploads, localDir)
		return testSourceUploadResult(localDir, "sources/current-tree.tar.gz"), nil
	}

	pinned := pinnedSourceTestJob(1, "/old/project-a", "sources/submitted.tar.gz")
	result, wasPinned, err := sourceUploadForReuse(context.Background(), &r2.Client{}, pinned, "/new/project-b")
	if err != nil {
		t.Fatalf("pinned sourceUploadForReuse: %v", err)
	}
	if !wasPinned || result.Manifest.Roots[0].R2Key != "sources/submitted.tar.gz" {
		t.Fatalf("pinned reuse result = %#v, pinned=%v", result, wasPinned)
	}
	if len(uploads) != 0 {
		t.Fatalf("pinned reuse re-derived current tree: uploads=%v", uploads)
	}
	mounts := sourceMountsFromManifest(result.Manifest, "/workspace/project-b")
	if len(mounts) != 1 || mounts[0].RemoteDir != "/workspace/project-b" || mounts[0].R2Key != "sources/submitted.tar.gz" {
		t.Fatalf("cross-project reuse mounts = %#v", mounts)
	}

	legacy := &db.Job{ID: 2, WorkingDir: "/new/project-b", Command: "python train.py"}
	result, wasPinned, err = sourceUploadForReuse(context.Background(), &r2.Client{}, legacy, legacy.WorkingDir)
	if err != nil {
		t.Fatalf("legacy sourceUploadForReuse: %v", err)
	}
	if wasPinned || result.Manifest.Roots[0].R2Key != "sources/current-tree.tar.gz" {
		t.Fatalf("legacy reuse result = %#v, pinned=%v", result, wasPinned)
	}
	if len(uploads) != 1 || uploads[0] != legacy.WorkingDir {
		t.Fatalf("legacy fallback uploads = %v, want %q", uploads, legacy.WorkingDir)
	}
}
