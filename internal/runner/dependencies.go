package runner

import (
	"fmt"
	"path/filepath"
	"strings"
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

// CheckDependencies checks whether a job's dependency spec is satisfied.
// Dependency spec format: "id1,id2:any,id3" where:
//   - "id" or "id:success" means dep must complete with exit 0
//   - "id:any" means dep just needs to complete (any exit code)
func CheckDependencies(depsSpec string, logDir string) DepCheckResult {
	if depsSpec == "" {
		return DepCheckResult{Result: DepOK}
	}

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

	return DepCheckResult{Result: DepOK}
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
