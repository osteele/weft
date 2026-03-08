package estimate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/r2"
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
	Name      string `json:"name"`
	Version   string `json:"version"`
	SizeBytes int64  `json:"size_bytes"`
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

// FetchUVManifests retrieves UV manifests from R2, using a local cache at
// ~/.cache/weft/uv-manifests/. Returns dir → manifest for each dir that
// has a lockfile hash with a matching manifest in R2.
func FetchUVManifests(r2Client *r2.Client, lockfileHashes map[string]string, platform string) map[string]*UVManifestRef {
	if r2Client == nil || len(lockfileHashes) == 0 {
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

	// Fetch from R2
	r2Key := fmt.Sprintf("uv-manifests/%s/%s.json", lockHash, platform)
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

// EstimateUVSyncBytes computes the total bytes that would need to be downloaded
// for a cold uv sync across all manifests. Packages are deduplicated by
// (name, version) so shared dependencies are only counted once.
func EstimateUVSyncBytes(manifests map[string]*UVManifestRef) int64 {
	type pkgKey struct{ name, version string }
	seen := make(map[pkgKey]int64)

	for _, m := range manifests {
		if m == nil {
			continue
		}
		for _, pkg := range m.Packages {
			k := pkgKey{pkg.Name, pkg.Version}
			if pkg.SizeBytes > seen[k] {
				seen[k] = pkg.SizeBytes
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
