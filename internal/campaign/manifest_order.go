package campaign

import "github.com/osteele/weft/internal/cloud"

// orderAgentJobsForManifest preserves the input order except where a
// same-payload CloudAfter edge requires producer-before-consumer.
// Cyclic refs are emitted at first traversal completion without dropping jobs.
func orderAgentJobsForManifest(jobs []cloud.AgentJob) {
	if len(jobs) < 2 {
		return
	}

	indexByID := make(map[int64]int, len(jobs))
	for i, job := range jobs {
		if _, exists := indexByID[job.ID]; !exists {
			indexByID[job.ID] = i
		}
	}

	hasEdge := false
	for consumerIdx, job := range jobs {
		for _, ref := range job.CloudAfter {
			if producerIdx, ok := indexByID[ref.JobID]; ok && producerIdx != consumerIdx {
				hasEdge = true
				break
			}
		}
		if hasEdge {
			break
		}
	}
	if !hasEdge {
		return
	}

	const (
		visiting = 1
		visited  = 2
	)
	state := make([]int, len(jobs))
	orderedIndexes := make([]int, 0, len(jobs))
	var visit func(int)
	visit = func(i int) {
		switch state[i] {
		case visited, visiting:
			return
		}
		state[i] = visiting
		for _, ref := range jobs[i].CloudAfter {
			producerIdx, ok := indexByID[ref.JobID]
			if !ok || producerIdx == i {
				continue
			}
			visit(producerIdx)
		}
		state[i] = visited
		orderedIndexes = append(orderedIndexes, i)
	}
	for i := range jobs {
		visit(i)
	}

	ordered := make([]cloud.AgentJob, len(jobs))
	for out, in := range orderedIndexes {
		ordered[out] = jobs[in]
	}
	copy(jobs, ordered)
}
