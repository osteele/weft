package estimate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// UVManifestRef is a minimal copy of runner.UVManifest for deserialization.
// We duplicate the type here to avoid a dependency cycle (estimate → runner).
type UVManifestRef struct {
	LockfileHash string         `json:"lockfile_hash"`
	Platform     string         `json:"platform"`
	CollectedAt  time.Time      `json:"collected_at"`
	Packages     []UVPackageRef `json:"packages"`
}

// UVPackageRef mirrors runner.UVPackage.
type UVPackageRef struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	SizeBytes      int64  `json:"size_bytes"`
	InstalledBytes int64  `json:"installed_bytes,omitempty"`
}

// LockfileHash computes SHA256 hashes for uv.lock files found in the given
// source directories. Returns a map of dir → "sha256:<hex>".
func LockfileHash(sourceDirs []string) map[string]string {
	result := make(map[string]string)
	for _, dir := range sourceDirs {
		lockPath := filepath.Join(dir, "uv.lock")
		data, err := os.ReadFile(lockPath)
		if err != nil {
			continue
		}
		h := sha256.Sum256(data)
		result[dir] = "sha256:" + hex.EncodeToString(h[:])
	}
	return result
}

// FetchUVManifests retrieves UV manifests, using a local cache at
// ~/.cache/weft/uv-manifests/ and falling back to R2 when a client is provided.
// Returns dir → manifest for each dir that has a matching cached or remote
// manifest for the requested platform.
func FetchUVManifests(r2Client *r2.Client, lockfileHashes map[string]string, platform string) map[string]*UVManifestRef {
	if len(lockfileHashes) == 0 {
		return nil
	}

	cacheBase := uvManifestCacheDir()
	var mu sync.Mutex
	var wg sync.WaitGroup
	result := make(map[string]*UVManifestRef)

	for dir, hash := range lockfileHashes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manifest := fetchOneManifest(r2Client, hash, platform, cacheBase)
			if manifest != nil {
				mu.Lock()
				result[dir] = manifest
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return result
}

func fetchOneManifest(r2Client *r2.Client, lockHash, platform, cacheBase string) *UVManifestRef {
	// Strip "sha256:" prefix for the cache path
	hashHex, _ := strings.CutPrefix(lockHash, "sha256:")

	// Check local cache first (immutable, never invalidated)
	cachePath := filepath.Join(cacheBase, hashHex, platform+".json")
	if data, err := os.ReadFile(cachePath); err == nil {
		var m UVManifestRef
		if json.Unmarshal(data, &m) == nil {
			return &m
		}
	}

	if r2Client == nil {
		return nil
	}

	// Fetch from R2
	r2Key := r2keys.UVManifest(lockHash, platform)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	data, err := r2Client.GetObject(ctx, r2Key)
	if err != nil {
		if !r2.IsNotFound(err) {
			log.Printf("uv manifest fetch %s: %v", r2Key, err)
		}
		return nil
	}

	var m UVManifestRef
	if err := json.Unmarshal(data, &m); err != nil {
		log.Printf("uv manifest parse %s: %v", r2Key, err)
		return nil
	}

	// Cache locally
	os.MkdirAll(filepath.Dir(cachePath), 0755)
	os.WriteFile(cachePath, data, 0644)

	return &m
}

// EstimateUVSyncBytes computes the total bytes needed for a cold uv sync disk
// footprint across all manifests. When installed_bytes is available, it is used
// instead of wheel size. Packages are deduplicated by (name, version), so
// shared dependencies are only counted once.
func EstimateUVSyncBytes(manifests map[string]*UVManifestRef) int64 {
	type pkgKey struct{ name, version string }
	seen := make(map[pkgKey]int64)

	for _, m := range manifests {
		if m == nil {
			continue
		}
		for _, pkg := range m.Packages {
			k := pkgKey{pkg.Name, pkg.Version}
			sz := pkg.SizeBytes
			if pkg.InstalledBytes > 0 {
				sz = pkg.InstalledBytes
			}
			if sz > seen[k] {
				seen[k] = sz
			}
		}
	}

	var total int64
	for _, sz := range seen {
		total += sz
	}
	return total
}

func uvManifestCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "weft", "uv-manifests")
}
