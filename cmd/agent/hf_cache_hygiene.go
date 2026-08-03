package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/runner"
)

// purgeUndeclaredHFAssets walks the HF Hub cache and deletes top-level
// model/dataset directories whose asset ref is not in the current
// workload's declared inputs. Empty declared input sets purge every HF
// Hub asset; see specs/campaign-lifecycle.allium rule HFCacheHygiene.
func purgeUndeclaredHFAssets(declared []string) (purged int, freed int64) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, 0
	}
	hubDir := filepath.Join(home, ".cache", "huggingface", "hub")
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		return 0, 0
	}

	keep := make(map[string]bool, len(declared))
	for _, ref := range declared {
		keep[ref] = true
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		asset, ok := dataloc.ParseHFDirName(entry.Name())
		if !ok {
			continue
		}
		if keep[asset.Ref()] {
			continue
		}
		dirPath := filepath.Join(hubDir, entry.Name())
		size := runner.DirSizeBytes(dirPath)
		if err := os.RemoveAll(dirPath); err != nil {
			fmt.Fprintf(os.Stderr, "hf cache hygiene: cannot remove %s: %v\n", dirPath, err)
			continue
		}
		purged++
		freed += size
		fmt.Printf("hf cache hygiene: purged %s (%s; not in current manifest)\n",
			asset.Ref(), formatBytes(size))
	}
	return purged, freed
}

func runHFCacheHygieneAtStartup(jobs []cloud.AgentJob) (purged int, freed int64) {
	declaredInputs := collectDeclaredInputs(jobs)
	purged, freed = purgeUndeclaredHFAssets(declaredInputs)
	if purged > 0 {
		fmt.Printf("hf cache hygiene: purged %d undeclared assets, freed %s\n",
			purged, formatBytes(freed))
	}
	return purged, freed
}

// collectDeclaredInputs returns the union of all jobs' Inputs in the
// current manifest, deduplicated.
func collectDeclaredInputs(jobs []cloud.AgentJob) []string {
	seen := make(map[string]bool)
	var out []string
	for _, j := range jobs {
		for _, in := range j.Inputs {
			if seen[in] {
				continue
			}
			seen[in] = true
			out = append(out, in)
		}
	}
	return out
}
