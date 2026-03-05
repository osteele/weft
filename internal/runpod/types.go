// Package runpod wraps the runpodctl CLI for searching GPU pods,
// creating instances, and managing their lifecycle.
package runpod

// Pod represents a Runpod pod from the API.
type Pod struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"desiredStatus"` // "RUNNING", "EXITED", etc.
	GPUType     string  `json:"gpuType"`
	GPUCount    int     `json:"gpuCount"`
	CostPerHour float64 `json:"costPerHr"`
	// SSH connection info (parsed from pod details)
	SSHHost string
	SSHPort int
}

// GPUType represents a Runpod GPU type from the marketplace.
type GPUType struct {
	ID             string  `json:"id"`
	DisplayName    string  `json:"displayName"`
	MemoryInGB     int     `json:"memoryInGb"`
	SecureCloud    bool    `json:"secureCloud"`
	CommunityCloud bool    `json:"communityCloud"`
	SecurePrice    float64 `json:"securePrice"`
	CommunityPrice float64 `json:"communityPrice"`
	LowestPrice    *LowestPrice
	MaxGPUCount    int `json:"maxGpuCount"`
}

// LowestPrice holds the cheapest available price for a GPU type.
type LowestPrice struct {
	MinimumBidPrice float64 `json:"minimumBidPrice"`
	Uninterruptable float64 `json:"uninterruptablePrice"`
}
