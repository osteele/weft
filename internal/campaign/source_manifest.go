package campaign

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/db"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

type sourceUploadSpec struct {
	inputs   []string
	commands []string
}

// pinnedSourceManifest returns the immutable submit-time manifest. A nil pin
// means the row predates source pinning and must use dispatch-time derivation.
func pinnedSourceManifest(job *db.Job) (weftsync.SourceManifest, bool, error) {
	if job == nil || job.Metadata == nil || job.Metadata.Source == nil || job.Metadata.Source.Pin == nil {
		return weftsync.SourceManifest{}, false, nil
	}
	pin := job.Metadata.Source.Pin
	if len(pin.Roots) == 0 {
		return weftsync.SourceManifest{}, true, fmt.Errorf("job %d pinned source manifest has no roots", job.ID)
	}
	manifest := weftsync.SourceManifest{Hash: pin.Hash, Roots: make([]weftsync.SourceRoot, 0, len(pin.Roots))}
	for i, root := range pin.Roots {
		if strings.TrimSpace(root.R2Key) == "" {
			return weftsync.SourceManifest{}, true, fmt.Errorf("job %d pinned source root %d has no R2 key", job.ID, i)
		}
		if strings.TrimSpace(root.MountBasename) == "" {
			return weftsync.SourceManifest{}, true, fmt.Errorf("job %d pinned source root %d has no mount basename", job.ID, i)
		}
		manifest.Roots = append(manifest.Roots, weftsync.SourceRoot{
			LocalPath:     root.LocalPath,
			MountBasename: root.MountBasename,
			MountRel:      root.MountRel,
			Hash:          root.Hash,
			R2Key:         root.R2Key,
			SizeBytes:     root.SizeBytes,
			Blobs:         append([]dataplane.SourceBlob(nil), root.Blobs...),
		})
	}
	return manifest, true, nil
}

func cloneJobSourceManifests(src map[int64]weftsync.SourceManifest) map[int64]weftsync.SourceManifest {
	if len(src) == 0 {
		return make(map[int64]weftsync.SourceManifest)
	}
	dst := make(map[int64]weftsync.SourceManifest, len(src))
	for jobID, manifest := range src {
		dst[jobID] = manifest
	}
	return dst
}

func planSourceStaging(groups []InstanceGroup) (map[string]sourceUploadSpec, map[int64]weftsync.SourceManifest, error) {
	uploads := make(map[string]sourceUploadSpec)
	pinned := make(map[int64]weftsync.SourceManifest)
	for _, group := range groups {
		for _, job := range group.Jobs {
			manifest, ok, err := pinnedSourceManifest(job)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				pinned[job.ID] = manifest
				continue
			}
			dir := workdir.ResolveLocal(job.EffectiveWorkingDir())
			if dir == "" || workdir.IsContainerPath(dir) {
				continue
			}
			spec := uploads[dir]
			spec.inputs = mergeStringSlices(spec.inputs, job.Inputs)
			spec.commands = mergeStringSlices(spec.commands, []string{job.Command})
			uploads[dir] = spec
		}
	}
	return uploads, pinned, nil
}
