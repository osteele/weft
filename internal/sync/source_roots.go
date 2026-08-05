package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataplane"
)

// SourceRoot describes one local source directory and its adjacent mount.
// See specs/source-data-sync.allium @guidance MultiRootSourceSnapshots.
type SourceRoot struct {
	LocalPath     string   `json:"local_path"`
	MountBasename string   `json:"mount_basename"`
	MountRel      string   `json:"mount_rel"`
	Hash          string   `json:"hash,omitempty"`
	R2Key         string   `json:"r2_key,omitempty"`
	SizeBytes     int64    `json:"size_bytes,omitempty"`
	VCS           *VCSInfo `json:"vcs,omitempty"`
}

// SourceManifest is the ordered source-root identity for one submitted job.
type SourceManifest struct {
	Roots []SourceRoot `json:"roots,omitempty"`
	Hash  string       `json:"hash,omitempty"`
}

// ResolveSourceRoots returns the project root followed by declared sibling
// roots, validating that each sibling mounts adjacent to the project root.
func ResolveSourceRoots(projectRoot string) ([]SourceRoot, error) {
	projectAbs, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root %s: %w", projectRoot, err)
	}
	info, err := os.Stat(projectAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("project root %s does not exist", projectAbs)
		}
		return nil, fmt.Errorf("stat project root %s: %w", projectAbs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("project root %s is not a directory", projectAbs)
	}

	roots := []SourceRoot{{
		LocalPath:     projectAbs,
		MountBasename: filepath.Base(projectAbs),
		MountRel:      ".",
	}}
	seenBasenames := map[string]string{filepath.Base(projectAbs): projectAbs}
	parent := filepath.Dir(projectAbs)

	for _, raw := range config.ProjectSiblingRoots(projectAbs) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		resolved := ExpandTildeDir(raw)
		if resolved == "" {
			resolved = raw
		}
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(projectAbs, resolved)
		}
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return nil, fmt.Errorf("resolve sibling root %q: %w", raw, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("sibling root %q resolved to %s, which does not exist", raw, abs)
			}
			return nil, fmt.Errorf("stat sibling root %q (%s): %w", raw, abs, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("sibling root %q resolved to %s, which is not a directory", raw, abs)
		}
		if filepath.Dir(abs) != parent {
			return nil, fmt.Errorf("sibling root %q resolved to %s; sibling roots must share parent %s with project root %s", raw, abs, parent, projectAbs)
		}
		base := filepath.Base(abs)
		if previous, ok := seenBasenames[base]; ok {
			return nil, fmt.Errorf("source root basename collision %q between %s and %s", base, previous, abs)
		}
		seenBasenames[base] = abs
		roots = append(roots, SourceRoot{
			LocalPath:     abs,
			MountBasename: base,
			MountRel:      ".." + string(filepath.Separator) + base,
		})
	}
	return roots, nil
}

// BuildSourceManifest hashes each root with the same excludes used for source
// sync, assigns each content-addressed R2 key, and hashes the ordered manifest.
func BuildSourceManifest(projectRoot string) (SourceManifest, []string, error) {
	return BuildSourceManifestForInputs(projectRoot, nil)
}

// BuildSourceManifestForInputs is BuildSourceManifest with declared local:
// inputs overlaid onto the project root only.
func BuildSourceManifestForInputs(projectRoot string, inputs []string) (SourceManifest, []string, error) {
	roots, err := ResolveSourceRoots(projectRoot)
	if err != nil {
		return SourceManifest{}, nil, err
	}
	tmpPaths := make([]string, 0, len(roots))
	cleanup := func() {}
	sourceDir := roots[0].LocalPath
	applyExcludes := true
	var stagedOverlayInputs []string
	if len(inputs) > 0 {
		stagedDir, overlayInputs, cleanupFn, err := stageSourceDirWithLocalInputs(roots[0].LocalPath, inputs)
		if err != nil {
			return SourceManifest{}, nil, err
		}
		cleanup = cleanupFn
		stagedOverlayInputs = overlayInputs
		if stagedDir != "" {
			sourceDir = stagedDir
			applyExcludes = false
		}
	}
	for i := range roots {
		rootSourceDir := roots[i].LocalPath
		if i == 0 {
			rootSourceDir = sourceDir
		}
		var (
			tmpPath string
			hash    string
		)
		if i == 0 && !applyExcludes {
			tmpPath, hash, err = createSourceTarballWithOverlays(rootSourceDir, nil, stagedOverlayInputs)
		} else {
			tmpPath, hash, err = CreateSourceTarball(rootSourceDir)
		}
		if err != nil {
			cleanup()
			removeFiles(tmpPaths)
			return SourceManifest{}, nil, fmt.Errorf("snapshot source root %s: %w", roots[i].LocalPath, err)
		}
		tmpPaths = append(tmpPaths, tmpPath)
		sizeBytes, err := IncludedSourceBytes(roots[i].LocalPath)
		if err != nil {
			cleanup()
			removeFiles(tmpPaths)
			return SourceManifest{}, nil, fmt.Errorf("measure source root %s: %w", roots[i].LocalPath, err)
		}
		roots[i].Hash = hash
		roots[i].R2Key = dataplane.SourceTarball(hash)
		roots[i].SizeBytes = sizeBytes
		if vcs := DetectVCSInfo(roots[i].LocalPath); vcs != nil {
			roots[i].VCS = vcs
		}
	}
	hash, err := manifestHash(roots)
	if err != nil {
		cleanup()
		removeFiles(tmpPaths)
		return SourceManifest{}, nil, err
	}
	cleanup()
	return SourceManifest{Roots: roots, Hash: hash}, tmpPaths, nil
}

func manifestHash(roots []SourceRoot) (string, error) {
	type rootIdentity struct {
		MountBasename string `json:"mount_basename"`
		Hash          string `json:"hash"`
		R2Key         string `json:"r2_key"`
	}
	ids := make([]rootIdentity, 0, len(roots))
	for _, root := range roots {
		ids = append(ids, rootIdentity{
			MountBasename: root.MountBasename,
			Hash:          root.Hash,
			R2Key:         root.R2Key,
		})
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("marshal source root manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func removeFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// TotalSourceRootBytes returns the included uncompressed bytes for all source roots.
func TotalSourceRootBytes(manifest *SourceManifest) int64 {
	if manifest == nil {
		return 0
	}
	var total int64
	for _, root := range manifest.Roots {
		total += root.SizeBytes
	}
	return total
}

// IncludedSourceBytes returns the uncompressed bytes that source sync would
// include for localDir.
func IncludedSourceBytes(localDir string) (int64, error) {
	excludes := sourceExcludes(localDir)
	var total int64
	err := filepath.Walk(localDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		if shouldExclude(relPath, info, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk %s: %w", localDir, err)
	}
	return total, nil
}

// AppendSiblingLocalPaths appends declared sibling-root paths to paths.
func AppendSiblingLocalPaths(projectRoot string, paths []string) ([]string, error) {
	roots, err := ResolveSourceRoots(projectRoot)
	if err != nil {
		return nil, err
	}
	for _, root := range roots[1:] {
		if !slices.Contains(paths, root.LocalPath) {
			paths = append(paths, root.LocalPath)
		}
	}
	return paths, nil
}
