package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/opsqueue"
)

// DepResult describes the outcome of checking a job's dependencies.
type DepResult int

const (
	// DepOK means all dependencies are satisfied.
	DepOK DepResult = iota
	// DepWaiting means at least one dependency hasn't completed yet.
	DepWaiting
	// DepFailed means a dependency failed (non-zero exit).
	DepFailed
)

// DepCheckResult holds the result of a dependency check.
type DepCheckResult struct {
	Result    DepResult
	FailedDep string // "depID:exitCode" when Result == DepFailed
}

// CheckDependencies checks whether a job's dependency spec and artifact needs are satisfied.
// Dependency spec format: "id1,id2:any,id3" where:
//   - "id" or "id:success" means dep must complete with exit 0
//   - "id:any" means dep just needs to complete (any exit code)
//
// Artifact needs format: "path:version" where version is the producer job ID.
// Both job-ID deps and artifact needs must be satisfied for DepOK.
func CheckDependencies(depsSpec string, needs []string, logDir string) DepCheckResult {
	return checkDependencies(depsSpec, needs, nil, logDir)
}

func checkJobDependencies(job *opsqueue.CommandJob, logDir string) DepCheckResult {
	if job == nil {
		return DepCheckResult{Result: DepOK}
	}
	return checkDependencies(job.Deps, job.Needs, job.ArtifactNeeds, logDir)
}

func checkDependencies(depsSpec string, needs []string, stageable []opsqueue.ArtifactNeed, logDir string) DepCheckResult {
	// Check job-ID based dependencies
	if depsSpec != "" {
		entries := strings.Split(depsSpec, ",")
		for _, entry := range entries {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}

			depID, mode := parseDepEntry(entry)

			// Find status file
			statusPath := filepath.Join(logDir, fmt.Sprintf("%s.status", depID))
			exitCode, found := ReadStatusFile(statusPath)
			if !found {
				// Check archived status files
				pattern := filepath.Join(logDir, fmt.Sprintf("%s-*.status", depID))
				matches, _ := filepath.Glob(pattern)
				if len(matches) > 0 {
					exitCode, found = ReadStatusFile(matches[0])
				}
			}

			if !found {
				return DepCheckResult{Result: DepWaiting}
			}

			if mode != "any" && exitCode != 0 {
				return DepCheckResult{
					Result:    DepFailed,
					FailedDep: fmt.Sprintf("%s:%d", depID, exitCode),
				}
			}
		}
	}

	// Check artifact-based dependencies
	for _, spec := range needs {
		parsed, err := ParseNeedsSpec(spec)
		if err != nil {
			// Invalid spec — treat as waiting (shouldn't happen if validated at CLI)
			return DepCheckResult{Result: DepWaiting}
		}

		if parsed.IsAsset() {
			if artifactNeedIsStageable(spec, stageable) {
				continue
			}
			satisfiedPath := NamedAssetSatisfiedFile(logDir, parsed.AssetName)
			exitCode, found := ReadStatusFile(satisfiedPath)
			if !found {
				return DepCheckResult{Result: DepWaiting}
			}
			if exitCode != 0 {
				return DepCheckResult{
					Result:    DepFailed,
					FailedDep: fmt.Sprintf("asset(%s):%d", parsed.AssetName, exitCode),
				}
			}
			continue
		}

		satisfiedPath := ArtifactSatisfiedFile(logDir, parsed.Path, parsed.Version)
		exitCode, found := ReadStatusFile(satisfiedPath)
		if !found {
			return DepCheckResult{Result: DepWaiting}
		}
		if exitCode != 0 {
			return DepCheckResult{
				Result:    DepFailed,
				FailedDep: fmt.Sprintf("artifact(%s@%d):%d", parsed.Path, parsed.Version, exitCode),
			}
		}
	}

	return DepCheckResult{Result: DepOK}
}

func artifactNeedIsStageable(spec string, needs []opsqueue.ArtifactNeed) bool {
	for _, need := range needs {
		if need.Spec == spec {
			return true
		}
	}
	return false
}

func writeArtifactNeedSatisfiedMarkers(logDir string, needs []opsqueue.ArtifactNeed) error {
	for _, need := range needs {
		parsed, err := ParseNeedsSpec(need.Spec)
		if err != nil {
			return fmt.Errorf("parse artifact need %q: %w", need.Spec, err)
		}
		if !parsed.IsAsset() {
			return fmt.Errorf("artifact need %q is not a named asset", need.Spec)
		}
		marker := NamedAssetSatisfiedFile(logDir, parsed.AssetName)
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			return fmt.Errorf("create artifact need marker directory: %w", err)
		}
		if err := os.WriteFile(marker, []byte("0\n"), 0o644); err != nil {
			return fmt.Errorf("write artifact need marker for %q: %w", need.Spec, err)
		}
	}
	return nil
}

// parseDepEntry parses "id:mode" or just "id" (defaults to "success").
func parseDepEntry(entry string) (id string, mode string) {
	parts := strings.SplitN(entry, ":", 2)
	id = parts[0]
	if len(parts) > 1 {
		mode = parts[1]
	} else {
		mode = "success"
	}
	return
}
