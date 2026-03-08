package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// UVPackage describes a single cached Python package.
type UVPackage struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SizeBytes int64  `json:"size_bytes"`
}

// UVManifest is a per-lockfile, per-platform snapshot of cached package sizes.
type UVManifest struct {
	LockfileHash string      `json:"lockfile_hash"`
	Platform     string      `json:"platform"`
	CollectedAt  time.Time   `json:"collected_at"`
	Packages     []UVPackage `json:"packages"`
}

// wheelNameRe matches wheel filenames: name-version-*.whl
// PEP 427: {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl
var wheelNameRe = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9._]*[A-Za-z0-9])?)-([^-]+)-`)

// CollectUVManifest reads uv.lock from workingDir, hashes it, walks the uv
// wheel cache to enumerate cached packages with sizes, and returns a manifest.
// Returns nil, nil if uv.lock does not exist.
func CollectUVManifest(workingDir string) (*UVManifest, error) {
	lockPath := filepath.Join(workingDir, "uv.lock")
	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read uv.lock: %w", err)
	}

	h := sha256.Sum256(lockData)
	lockHash := "sha256:" + hex.EncodeToString(h[:])

	cacheDir := uvCacheDir()
	packages := scanUVCache(cacheDir)

	return &UVManifest{
		LockfileHash: lockHash,
		Platform:     runtime.GOOS + "-" + runtime.GOARCH,
		CollectedAt:  time.Now().UTC(),
		Packages:     packages,
	}, nil
}

// WriteUVManifest serializes a manifest to <logDir>/uv-manifest.json.
func WriteUVManifest(logDir string, manifest *UVManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(logDir, "uv-manifest.json"), data, 0644)
}

// uvCacheDir returns the uv cache directory.
func uvCacheDir() string {
	if d := os.Getenv("UV_CACHE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "uv")
}

// scanUVCache walks the uv cache's wheels-v* subdirectories looking for
// wheel files and aggregates sizes by (name, version).
func scanUVCache(cacheDir string) []UVPackage {
	type pkgKey struct{ name, version string }
	sizes := make(map[pkgKey]int64)

	// Only walk wheels-v* subdirs to avoid traversing unrelated cache data
	wheelsDirs, _ := filepath.Glob(filepath.Join(cacheDir, "wheels-v*"))
	for _, wheelsDir := range wheelsDirs {
		filepath.WalkDir(wheelsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".whl") {
				return nil
			}
			m := wheelNameRe.FindStringSubmatch(d.Name())
			if m == nil {
				return nil
			}
			name := normalizePackageName(m[1])
			version := m[2]

			info, err := d.Info()
			if err != nil {
				return nil
			}
			sizes[pkgKey{name, version}] += info.Size()
			return nil
		})
	}

	packages := make([]UVPackage, 0, len(sizes))
	for k, sz := range sizes {
		packages = append(packages, UVPackage{
			Name:      k.name,
			Version:   k.version,
			SizeBytes: sz,
		})
	}
	return packages
}

// normalizePackageName converts a wheel distribution name to a normalized form
// (PEP 503): lowercase, runs of [-_.] replaced with a single dash.
func normalizePackageName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	prevDash := false
	for _, c := range name {
		if c == '-' || c == '_' || c == '.' {
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		} else {
			b.WriteRune(c)
			prevDash = false
		}
	}
	return b.String()
}

// collectAndWriteUVManifest collects the UV package manifest from the cache
// and writes it to the log directory. Used by both single-job and queue runners.
func collectAndWriteUVManifest(jobID int64, workingDir, logDir string) {
	manifest, err := CollectUVManifest(workingDir)
	if err != nil {
		log.Printf("Job %d: uv manifest collection failed: %v", jobID, err)
		return
	}
	if manifest == nil {
		return
	}
	if err := WriteUVManifest(logDir, manifest); err != nil {
		log.Printf("Job %d: uv manifest write failed: %v", jobID, err)
	} else {
		log.Printf("Job %d: collected uv manifest (%d packages)", jobID, len(manifest.Packages))
	}
}
