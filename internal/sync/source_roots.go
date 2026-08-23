package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/dataplane"
)

const (
	SourceRootOriginProject  = "project"
	SourceRootOriginExplicit = "explicit"
	SourceRootOriginDerived  = "derived from tool.uv.sources"
)

// SourceRoot describes one local source directory and its adjacent mount.
// See specs/source-data-sync.allium @guidance MultiRootSourceSnapshots.
type SourceRoot struct {
	LocalPath     string                 `json:"local_path"`
	MountBasename string                 `json:"mount_basename"`
	MountRel      string                 `json:"mount_rel"`
	Origins       []string               `json:"origins,omitempty"`
	Hash          string                 `json:"hash,omitempty"`
	R2Key         string                 `json:"r2_key,omitempty"`
	SizeBytes     int64                  `json:"size_bytes,omitempty"`
	Blobs         []dataplane.SourceBlob `json:"blobs,omitempty"`
	VCS           *VCSInfo               `json:"vcs,omitempty"`
}

type SourceRootWarning struct {
	Path    string `json:"path,omitempty"`
	Origin  string `json:"origin,omitempty"`
	Message string `json:"message,omitempty"`
}

// SourceManifest is the ordered source-root identity for one submitted job.
type SourceManifest struct {
	Roots    []SourceRoot        `json:"roots,omitempty"`
	Hash     string              `json:"hash,omitempty"`
	Warnings []SourceRootWarning `json:"warnings,omitempty"`
}

// ResolveSourceRoots returns the project root followed by declared sibling
// roots, validating that each sibling mounts adjacent to the project root.
func ResolveSourceRoots(projectRoot string) ([]SourceRoot, error) {
	roots, _, err := ResolveSourceRootsForCommands(projectRoot, nil)
	return roots, err
}

func ResolveSourceRootsForCommands(projectRoot string, commands []string) ([]SourceRoot, []SourceRootWarning, error) {
	projectAbs, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve project root %s: %w", projectRoot, err)
	}
	info, err := os.Stat(projectAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("project root %s does not exist", projectAbs)
		}
		return nil, nil, fmt.Errorf("stat project root %s: %w", projectAbs, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("project root %s is not a directory", projectAbs)
	}

	roots := []SourceRoot{{
		LocalPath:     projectAbs,
		MountBasename: filepath.Base(projectAbs),
		MountRel:      ".",
		Origins:       []string{SourceRootOriginProject},
	}}
	seenBasenames := map[string]string{filepath.Base(projectAbs): projectAbs}
	seenCanonical := map[string]int{}
	if canonical, err := canonicalRootPath(projectAbs); err == nil {
		seenCanonical[canonical] = 0
	}
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
			return nil, nil, fmt.Errorf("resolve sibling root %q: %w", raw, err)
		}
		root, canonical, err := validateExplicitSourceRoot(raw, abs, projectAbs, parent)
		if err != nil {
			return nil, nil, err
		}
		if previous, ok := seenBasenames[root.MountBasename]; ok {
			return nil, nil, fmt.Errorf("source root basename collision %q between %s and %s", root.MountBasename, previous, abs)
		}
		if _, ok := seenCanonical[canonical]; ok {
			return nil, nil, fmt.Errorf("source root %q resolved to %s, which duplicates an existing explicit root", raw, abs)
		}
		seenBasenames[root.MountBasename] = abs
		seenCanonical[canonical] = len(roots)
		roots = append(roots, root)
	}

	derived, err := dataloc.ScanUVPathSources(projectAbs, commands)
	if err != nil {
		return nil, nil, err
	}
	var warnings []SourceRootWarning
	for _, source := range derived {
		root, canonical, warning := validateDerivedSourceRoot(projectAbs, parent, source)
		if warning != nil {
			warnings = append(warnings, *warning)
			continue
		}
		if root.LocalPath == "" {
			continue
		}
		if idx, ok := seenCanonical[canonical]; ok {
			roots[idx].Origins = appendOrigin(roots[idx].Origins, SourceRootOriginDerived)
			continue
		}
		if previous, ok := seenBasenames[root.MountBasename]; ok {
			warnings = append(warnings, SourceRootWarning{
				Path:    source.Path,
				Origin:  SourceRootOriginDerived,
				Message: fmt.Sprintf("derived path dependency %q from %s resolves to %s, whose mount basename collides with %s; it will not be synced", source.Path, source.File, root.LocalPath, previous),
			})
			continue
		}
		seenBasenames[root.MountBasename] = root.LocalPath
		seenCanonical[canonical] = len(roots)
		roots = append(roots, root)
	}
	return roots, warnings, nil
}

func validateExplicitSourceRoot(raw, abs, projectAbs, parent string) (SourceRoot, string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceRoot{}, "", fmt.Errorf("sibling root %q resolved to %s, which does not exist", raw, abs)
		}
		return SourceRoot{}, "", fmt.Errorf("stat sibling root %q (%s): %w", raw, abs, err)
	}
	if !info.IsDir() {
		return SourceRoot{}, "", fmt.Errorf("sibling root %q resolved to %s, which is not a directory", raw, abs)
	}
	if filepath.Dir(abs) != parent {
		return SourceRoot{}, "", fmt.Errorf("sibling root %q resolved to %s; sibling roots must share parent %s with project root %s", raw, abs, parent, projectAbs)
	}
	canonical, err := canonicalRootPath(abs)
	if err != nil {
		return SourceRoot{}, "", fmt.Errorf("canonicalize sibling root %q (%s): %w", raw, abs, err)
	}
	base := filepath.Base(abs)
	return SourceRoot{
		LocalPath:     abs,
		MountBasename: base,
		MountRel:      ".." + string(filepath.Separator) + base,
		Origins:       []string{SourceRootOriginExplicit},
	}, canonical, nil
}

func validateDerivedSourceRoot(projectAbs, parent string, source dataloc.UVPathSource) (SourceRoot, string, *SourceRootWarning) {
	resolved := source.Path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(source.Base, resolved)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return SourceRoot{}, "", derivedSourceWarning(source, fmt.Sprintf("could not be resolved: %v", err))
	}
	if pathInsideOrSame(projectAbs, abs) {
		return SourceRoot{}, "", nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceRoot{}, "", derivedSourceWarning(source, fmt.Sprintf("resolves to %s, which does not exist; it will not be synced", abs))
		}
		return SourceRoot{}, "", derivedSourceWarning(source, fmt.Sprintf("resolves to %s but could not be inspected: %v; it will not be synced", abs, err))
	}
	if !info.IsDir() {
		return SourceRoot{}, "", derivedSourceWarning(source, fmt.Sprintf("resolves to %s, which is not a directory; it will not be synced", abs))
	}
	if filepath.Dir(abs) != parent {
		return SourceRoot{}, "", derivedSourceWarning(source, fmt.Sprintf("resolves to %s, which is outside the project but is not a sibling of %s; it will not be synced", abs, projectAbs))
	}
	canonical, err := canonicalRootPath(abs)
	if err != nil {
		return SourceRoot{}, "", derivedSourceWarning(source, fmt.Sprintf("resolves to %s but could not be canonicalized: %v; it will not be synced", abs, err))
	}
	base := filepath.Base(abs)
	return SourceRoot{
		LocalPath:     abs,
		MountBasename: base,
		MountRel:      ".." + string(filepath.Separator) + base,
		Origins:       []string{SourceRootOriginDerived},
	}, canonical, nil
}

func derivedSourceWarning(source dataloc.UVPathSource, reason string) *SourceRootWarning {
	return &SourceRootWarning{
		Path:    source.Path,
		Origin:  SourceRootOriginDerived,
		Message: fmt.Sprintf("derived path dependency %q from tool.uv.sources in %s %s", source.Path, source.File, reason),
	}
}

func canonicalRootPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func pathInsideOrSame(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func appendOrigin(origins []string, origin string) []string {
	if slices.Contains(origins, origin) {
		return origins
	}
	return append(origins, origin)
}

// BuildSourceManifest hashes each root with the same excludes used for source
// sync, assigns each content-addressed R2 key, and hashes the ordered manifest.
func BuildSourceManifest(projectRoot string) (SourceManifest, []string, error) {
	return BuildSourceManifestForInputs(projectRoot, nil)
}

// BuildSourceManifestForInputs is BuildSourceManifest with declared local:
// inputs overlaid onto the project root only.
func BuildSourceManifestForInputs(projectRoot string, inputs []string) (SourceManifest, []string, error) {
	return BuildSourceManifestForInputsAndCommands(projectRoot, inputs, nil)
}

func BuildSourceManifestForInputsAndCommands(projectRoot string, inputs []string, commands []string) (SourceManifest, []string, error) {
	build, err := buildSourceManifestForInputsAndCommands(projectRoot, inputs, commands, false)
	if err != nil {
		return SourceManifest{}, nil, err
	}
	build.cleanup()
	return build.manifest, build.tarPaths, nil
}

// BuildCloudSourceManifestForInputsAndCommands builds the cloud snapshot
// identity after diverting large files from each root's residual tarball.
func BuildCloudSourceManifestForInputsAndCommands(projectRoot string, inputs []string, commands []string) (SourceManifest, []string, error) {
	build, err := buildSourceManifestForInputsAndCommands(projectRoot, inputs, commands, true)
	if err != nil {
		return SourceManifest{}, nil, err
	}
	build.cleanup()
	return build.manifest, build.tarPaths, nil
}

type sourceManifestBuild struct {
	manifest  SourceManifest
	tarPaths  []string
	blobPaths [][]string
	workers   int
	cleanup   func()
}

func buildSourceManifestForInputsAndCommands(projectRoot string, inputs []string, commands []string, divertLargeFiles bool) (sourceManifestBuild, error) {
	workers := sourceWorkerLimit()
	roots, warnings, err := ResolveSourceRootsForCommands(projectRoot, commands)
	if err != nil {
		return sourceManifestBuild{}, err
	}
	tmpPaths := make([]string, 0, len(roots))
	blobPaths := make([][]string, 0, len(roots))
	cleanup := func() {}
	sourceDir := roots[0].LocalPath
	applyExcludes := true
	var stagedOverlayInputs []string
	if len(inputs) > 0 {
		var stagedDir string
		var overlayInputs []string
		var cleanupFn func()
		if divertLargeFiles {
			stagedDir, overlayInputs, cleanupFn, err = stageSourceDirWithLocalInputsLimit(roots[0].LocalPath, inputs, 0)
		} else {
			stagedDir, overlayInputs, cleanupFn, err = stageSourceDirWithLocalInputs(roots[0].LocalPath, inputs)
		}
		if err != nil {
			return sourceManifestBuild{}, err
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
		excludes := sourceExcludes(rootSourceDir)
		if i == 0 && !applyExcludes {
			excludes = nil
		}
		var omittedPaths map[string]struct{}
		var rootBlobPaths []string
		if divertLargeFiles {
			var blobs []dataplane.SourceBlob
			blobs, rootBlobPaths, omittedPaths, err = findLargeSourceBlobs(rootSourceDir, excludes, workers)
			roots[i].Blobs = blobs
		}
		if err == nil && divertLargeFiles {
			tmpPath, hash, err = createSourceTarballWithWorkers(rootSourceDir, excludes, stagedOverlayInputs, omittedPaths, MaxSourceTarballBytes, workers)
		} else if err == nil && i == 0 && !applyExcludes {
			tmpPath, hash, err = createSourceTarballWithWorkers(rootSourceDir, nil, stagedOverlayInputs, nil, MaxSourceTarballBytes, workers)
		} else {
			if err == nil {
				tmpPath, hash, err = createSourceTarballWithWorkers(rootSourceDir, excludes, nil, nil, MaxSourceTarballBytes, workers)
			}
		}
		if err != nil {
			cleanup()
			removeFiles(tmpPaths)
			return sourceManifestBuild{}, fmt.Errorf("snapshot source root %s: %w", roots[i].LocalPath, err)
		}
		tmpPaths = append(tmpPaths, tmpPath)
		blobPaths = append(blobPaths, rootBlobPaths)
		sizeBytes, err := includedSourceBytes(rootSourceDir, excludes)
		if err != nil {
			cleanup()
			removeFiles(tmpPaths)
			return sourceManifestBuild{}, fmt.Errorf("measure source root %s: %w", roots[i].LocalPath, err)
		}
		roots[i].Hash = hash
		roots[i].R2Key = dataplane.SourceTarballV2(hash)
		roots[i].SizeBytes = sizeBytes
		if vcs := DetectVCSInfo(roots[i].LocalPath); vcs != nil {
			roots[i].VCS = vcs
		}
	}
	hash, err := manifestHash(roots)
	if err != nil {
		cleanup()
		removeFiles(tmpPaths)
		return sourceManifestBuild{}, err
	}
	return sourceManifestBuild{
		manifest:  SourceManifest{Roots: roots, Hash: hash, Warnings: warnings},
		tarPaths:  tmpPaths,
		blobPaths: blobPaths,
		workers:   workers,
		cleanup:   cleanup,
	}, nil
}

func manifestHash(roots []SourceRoot) (string, error) {
	ids := make([]dataplane.SourceManifestRoot, 0, len(roots))
	for _, root := range roots {
		ids = append(ids, dataplane.SourceManifestRoot{
			MountBasename: root.MountBasename,
			Hash:          root.Hash,
			R2Key:         root.R2Key,
			Blobs:         root.Blobs,
		})
	}
	return dataplane.SourceManifestSHA256(ids)
}

func findLargeSourceBlobs(localDir string, excludes []string, workerLimit int) ([]dataplane.SourceBlob, []string, map[string]struct{}, error) {
	type candidate struct {
		filename string
		relPath  string
	}
	var candidates []candidate
	err := filepath.Walk(localDir, func(filename string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsPermission(walkErr) {
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return walkErr
		}
		relPath, err := filepath.Rel(localDir, filename)
		if err != nil {
			return err
		}
		if shouldExclude(relPath, info, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < LargeSourceBlobThresholdBytes {
			return nil
		}
		candidates = append(candidates, candidate{filename: filename, relPath: filepath.ToSlash(relPath)})
		return nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("scan large source files in %s: %w", localDir, err)
	}
	if len(candidates) == 0 {
		return nil, nil, map[string]struct{}{}, nil
	}

	type hashResult struct {
		hash string
		err  error
	}
	results := make([]hashResult, len(candidates))
	jobs := make(chan int)
	workers := min(max(1, workerLimit), len(candidates))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				item := candidates[index]
				results[index].hash, results[index].err = hashSourceBlob(item.filename, item.relPath)
			}
		}()
	}
	for index := range candidates {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	blobs := make([]dataplane.SourceBlob, 0, len(candidates))
	paths := make([]string, 0, len(candidates))
	omitted := make(map[string]struct{}, len(candidates))
	for index, item := range candidates {
		result := results[index]
		if result.err != nil {
			return nil, nil, nil, result.err
		}
		blobs = append(blobs, dataplane.SourceBlob{
			R2Key:   dataplane.NamedAsset(result.hash),
			RelPath: item.relPath,
			SHA256:  result.hash,
		})
		paths = append(paths, item.filename)
		omitted[item.relPath] = struct{}{}
	}
	return blobs, paths, omitted, nil
}

func hashSourceBlob(filename, relPath string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open diverted source file %s: %w", relPath, err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return "", fmt.Errorf("hash diverted source file %s: %w", relPath, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close diverted source file %s: %w", relPath, closeErr)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
	return includedSourceBytes(localDir, sourceExcludes(localDir))
}

func includedSourceBytes(localDir string, excludes []string) (int64, error) {
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
