package sync

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/workdir"
)

type LocalOverlay struct {
	Input string
	Abs   string
	Rel   string
	IsDir bool
}

type MissingLocalInputPolicy int

const (
	RequireLocalInput MissingLocalInputPolicy = iota
	SkipMissingLocalInput
)

func LocalInputOverlays(localDir string, inputs []string, missingPolicy MissingLocalInputPolicy) ([]LocalOverlay, error) {
	localDir, err := filepath.Abs(localDir)
	if err != nil {
		return nil, fmt.Errorf("resolve source directory: %w", err)
	}
	var overlays []LocalOverlay
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
		info, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) && missingPolicy == SkipMissingLocalInput {
				slog.Debug("skipping absent local input", "component", "sync", "input", ref, "resolved", candidate)
				continue
			}
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
			"input", ref, "abs", candidate, "rel", rel, "isDir", info.IsDir())
		overlays = append(overlays, LocalOverlay{
			Input: ref,
			Abs:   candidate,
			Rel:   rel,
			IsDir: info.IsDir(),
		})
	}
	slices.SortFunc(overlays, func(a, b LocalOverlay) int {
		return strings.Compare(a.Rel, b.Rel)
	})
	return overlays, nil
}

func nonLocalInputs(inputs []string) []string {
	out := make([]string, 0, len(inputs))
	for _, input := range inputs {
		if strings.HasPrefix(input, "local:") {
			continue
		}
		out = append(out, input)
	}
	return out
}

// CollectExtraPaths gathers file paths to sync from input flags and .weft.toml.
// It classifies inputs into asset refs (ignored here) and file paths,
// then merges with extra_paths from the project config if found.
// Relative paths (e.g., from local: inputs) are resolved against localDir
// and converted to tilde-relative form for portable local/remote syncing.
//
// Paths that do not exist on the laptop are dropped silently. local: inputs
// are commonly outputs of prior on-prem jobs that live only on the remote
// host; treating them as "present at this relative path on whichever host
// runs the job" lets the host-sync push the job without the rsync side
// faulting on missing local sources. If a declared path is missing on the
// remote too, the job will fail at runtime — that is the right place to
// surface the error.
func CollectExtraPaths(inputs []string, localDir string) []string {
	_, filePaths := dataloc.ClassifyInputs(inputs)
	filePaths = append(filePaths, config.ProjectExtraPaths(localDir)...)
	out := filePaths[:0]
	for _, p := range filePaths {
		if p == "" {
			continue
		}
		abs := resolveLocalAbs(p, localDir)
		if abs == "" {
			out = append(out, p)
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			if os.IsNotExist(err) {
				slog.Debug("skipping absent local input", "component", "sync", "path", p, "resolved", abs)
				continue
			}
		}
		if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~/") {
			out = append(out, workdir.ToTildeRelative(abs))
			continue
		}
		out = append(out, p)
	}
	return out
}

// resolveLocalAbs returns the absolute on-laptop path for a CollectExtraPaths
// candidate. Returns "" when the path cannot be resolved (no usable home dir
// for a tilde path); callers should fall through and keep the original.
func resolveLocalAbs(p, localDir string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, p[2:])
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(localDir, p)
}
