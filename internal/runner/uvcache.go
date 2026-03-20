package runner

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// UVPackage describes a single cached Python package.
type UVPackage struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	SizeBytes      int64  `json:"size_bytes"`
	InstalledBytes int64  `json:"installed_bytes,omitempty"`
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
var distInfoDirRe = regexp.MustCompile(`^(.+)-([^-]+)\.dist-info$`)

// CollectUVManifest reads uv.lock from workingDir, hashes it, walks the uv
// wheel cache to enumerate cached packages, and augments entries with installed
// sizes from .venv site-packages when available. Returns nil, nil if uv.lock
// does not exist.
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
	installedSizes := scanInstalledSitePackages(workingDir)
	for i := range packages {
		key := uvPkgKey{name: packages[i].Name, version: packages[i].Version}
		if installed := installedSizes[key]; installed > 0 {
			packages[i].InstalledBytes = installed
		}
	}

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
	sizes := make(map[uvPkgKey]int64)

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
			sizes[uvPkgKey{name, version}] += info.Size()
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

type uvPkgKey struct {
	name    string
	version string
}

// scanInstalledSitePackages collects installed package sizes from
// .venv/lib/python*/site-packages by reading each dist-info RECORD.
func scanInstalledSitePackages(workingDir string) map[uvPkgKey]int64 {
	pattern := filepath.Join(workingDir, ".venv", "lib", "python*", "site-packages")
	sitePackageDirs, _ := filepath.Glob(pattern)
	if len(sitePackageDirs) == 0 {
		return nil
	}
	sort.Strings(sitePackageDirs)

	sizes := make(map[uvPkgKey]int64)
	for _, sitePackages := range sitePackageDirs {
		entries, err := os.ReadDir(sitePackages)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			m := distInfoDirRe.FindStringSubmatch(entry.Name())
			if m == nil {
				continue
			}
			key := uvPkgKey{
				name:    normalizePackageName(m[1]),
				version: m[2],
			}
			distInfoDir := filepath.Join(sitePackages, entry.Name())
			size, ok := sizeFromDistInfoRecord(sitePackages, distInfoDir)
			if !ok {
				size = DirSizeBytes(distInfoDir)
			}
			if size > sizes[key] {
				sizes[key] = size
			}
		}
	}
	return sizes
}

func sizeFromDistInfoRecord(sitePackages, distInfoDir string) (int64, bool) {
	recordPath := filepath.Join(distInfoDir, "RECORD")
	f, err := os.Open(recordPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1

	var total int64
	seen := make(map[string]struct{})
	for {
		row, err := reader.Read()
		if err != nil {
			break
		}
		if len(row) == 0 {
			continue
		}
		relPath := strings.TrimSpace(row[0])
		if relPath == "" {
			continue
		}
		absPath := filepath.Clean(filepath.Join(sitePackages, filepath.FromSlash(relPath)))
		if !isWithinDir(absPath, sitePackages) {
			continue
		}
		if _, ok := seen[absPath]; ok {
			continue
		}
		seen[absPath] = struct{}{}
		info, err := os.Stat(absPath)
		if err != nil || info.IsDir() {
			continue
		}
		total += info.Size()
	}
	if total == 0 {
		return 0, false
	}
	return total, true
}

func isWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
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
