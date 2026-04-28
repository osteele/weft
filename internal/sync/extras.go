package sync

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/workdir"
)

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
