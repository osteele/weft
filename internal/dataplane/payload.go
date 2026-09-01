package dataplane

// JobPayload identifies immutable bytes admitted with a logical job.
type JobPayload struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	R2Key     string `json:"r2_key"`
}
