package ops

// PublishedKeyBelongsToAttempt reports whether a publication row's payload
// key lies under the producing job attempt's outputs or artifact-files
// prefix. Every consumer of attempt_publication_artifacts.payload_key applies
// this ownership check before treating the key as the artifact's R2 backing.
func PublishedKeyBelongsToAttempt(key string, jobID, runID int64) bool {
	return publishedKeyBelongsToAttempt(key, jobID, runID)
}
