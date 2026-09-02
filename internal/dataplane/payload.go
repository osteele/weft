package dataplane

// JobPayload identifies immutable bytes admitted with a logical job.
type JobPayload struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	R2Key     string `json:"r2_key"`
}

// ArtifactNeed identifies an R2-backed dependency and its workspace-relative
// destination. Controllers resolve names and producer attempts before dispatch
// so agents can materialize the bytes without access to controller state.
type ArtifactNeed struct {
	Spec        string `json:"spec,omitempty"`
	Path        string `json:"path"`
	R2Key       string `json:"r2_key"`
	ContentType string `json:"content_type,omitempty"`
}
