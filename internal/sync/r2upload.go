package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

// UploadSourceProgressFunc receives coarse source upload phase updates.
// Phases are descriptive labels such as "hashing source", "checking cache",
// "uploading source", and "ready".
type UploadSourceProgressFunc func(phase string)

// UploadSourceToR2 creates a content-addressed tarball of localDir and uploads
// it to R2 if it doesn't already exist. Returns the R2 key.
func UploadSourceToR2(ctx context.Context, r2Client *r2.Client, localDir string) (string, error) {
	return UploadSourceToR2ForInputs(ctx, r2Client, localDir, nil)
}

// UploadSourceToR2ForInputs creates a content-addressed tarball for localDir,
// ensuring declared local: inputs are included even if the source snapshot would
// normally exclude them.
func UploadSourceToR2ForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string) (string, error) {
	return UploadSourceToR2WithProgressForInputs(ctx, r2Client, localDir, inputs, nil)
}

// UploadSourceToR2WithProgress creates a content-addressed tarball of localDir,
// uploads it to R2 if needed, and reports coarse phase changes via onProgress.
func UploadSourceToR2WithProgress(ctx context.Context, r2Client *r2.Client, localDir string, onProgress UploadSourceProgressFunc) (string, error) {
	return UploadSourceToR2WithProgressForInputs(ctx, r2Client, localDir, nil, onProgress)
}

// UploadSourceToR2WithProgressForInputs creates a content-addressed tarball of
// localDir, overlays declared local: inputs when needed, uploads it to R2 if
// needed, and reports coarse phase changes via onProgress.
func UploadSourceToR2WithProgressForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, onProgress UploadSourceProgressFunc) (string, error) {
	if onProgress == nil {
		onProgress = func(string) {}
	}

	sourceDir := localDir
	applyExcludes := true
	cleanup := func() {}
	if len(inputs) > 0 {
		onProgress("staging explicit inputs")
		slog.Info("source overlay: staging inputs", "component", "sync",
			"localDir", localDir, "inputCount", len(inputs), "inputs", inputs)
		stagedDir, cleanupFn, err := stageSourceDirWithLocalInputs(localDir, inputs)
		if err != nil {
			return "", err
		}
		if stagedDir != "" {
			sourceDir = stagedDir
			applyExcludes = false
			cleanup = cleanupFn
			slog.Info("source overlay: staged directory created", "component", "sync",
				"stagedDir", stagedDir)
		} else {
			slog.Info("source overlay: no local overlays needed", "component", "sync",
				"localDir", localDir)
		}
	}
	defer cleanup()

	onProgress("hashing source")
	var (
		tmpPath string
		hash    string
		err     error
	)
	if applyExcludes {
		tmpPath, hash, err = CreateSourceTarball(sourceDir)
	} else {
		tmpPath, hash, err = createSourceTarball(sourceDir, nil)
	}
	if err != nil {
		return "", fmt.Errorf("create source tarball: %w", err)
	}
	defer os.Remove(tmpPath)

	slog.Info("source overlay: tarball created", "component", "sync",
		"hash", hash, "applyExcludes", applyExcludes, "sourceDir", sourceDir)

	key := dataplane.SourceTarball(hash)

	onProgress("checking cache")
	exists, err := r2Client.ObjectExists(ctx, key)
	if err != nil {
		return "", fmt.Errorf("check source exists: %w", err)
	}
	if exists {
		onProgress("ready")
		return key, nil
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return "", fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()

	onProgress("uploading source")
	if err := r2Client.PutObject(ctx, key, f, "application/gzip"); err != nil {
		return "", fmt.Errorf("upload source tarball: %w", err)
	}

	onProgress("ready")
	return key, nil
}

func stageSourceDirWithLocalInputs(localDir string, inputs []string) (string, func(), error) {
	localDir, err := filepath.Abs(localDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve source directory: %w", err)
	}
	overlays, err := localInputOverlays(localDir, inputs)
	if err != nil {
		return "", nil, err
	}
	if len(overlays) == 0 {
		return "", func() {}, nil
	}
	return buildSourceSnapshotWithOverlays(localDir, overlays)
}

type localOverlay struct {
	input string
	abs   string
	rel   string
}

func localInputOverlays(localDir string, inputs []string) ([]localOverlay, error) {
	var overlays []localOverlay
	seen := map[string]struct{}{}

	for _, ref := range inputs {
		if !strings.HasPrefix(ref, "local:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(ref, "local:"))
		if raw == "" {
			return nil, fmt.Errorf("invalid input %q: local path is empty", ref)
		}

		candidate := filepath.Clean(filepath.Join(localDir, raw))
		rel, err := filepath.Rel(localDir, candidate)
		if err != nil {
			return nil, fmt.Errorf("resolve input %q: %w", ref, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("input %q escapes project directory %s", ref, localDir)
		}
		if _, err := os.Stat(candidate); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("declared local input %q not found at %s", ref, candidate)
			}
			return nil, fmt.Errorf("check declared local input %q: %w", ref, err)
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		slog.Debug("source overlay: resolved local input", "component", "sync",
			"input", ref, "abs", candidate, "rel", rel)
		overlays = append(overlays, localOverlay{
			input: ref,
			abs:   candidate,
			rel:   rel,
		})
	}

	slices.SortFunc(overlays, func(a, b localOverlay) int {
		return strings.Compare(a.rel, b.rel)
	})
	return overlays, nil
}
