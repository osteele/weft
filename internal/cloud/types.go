// Package cloud provides provider-neutral types and interfaces for cloud GPU providers.
package cloud

import "time"

// Provider identifies a cloud GPU provider.
type Provider string

const (
	ProviderVastai Provider = "vastai"
	ProviderRunpod Provider = "runpod"
)

// Offer represents a GPU rental offer from any cloud provider.
type Offer struct {
	ProviderID        string   // provider-specific offer ID
	Provider          Provider // which provider
	GPUName           string   // e.g., "RTX_4090"
	NumGPUs           int
	GPUMemGB          float64 // per-GPU memory in GB
	CostPerHour       float64 // $/hr for the whole instance
	Reliability       float64 // 0-1
	DLPerf            float64 // deep learning perf score
	DataCenter        string  // e.g., "US-East"
	CUDAVersion       float64 // max supported CUDA version
	DiskSpaceGB       float64 // GB available
	DownloadBandwidth float64 // Mbps
	UploadBandwidth   float64 // Mbps
	Verified          bool
}

// Instance represents a running cloud instance from any provider.
type Instance struct {
	ProviderID  string // provider-specific instance ID
	Provider    Provider
	Status      string // "running", "loading", "exited", etc.
	SSHHost     string
	SSHPort     int
	CostPerHour float64
	DataCenter  string
}

// OfferConstraints describes what GPU capabilities a job needs.
type OfferConstraints struct {
	GPUClass       string  // e.g., "RTX_4090", "A100"
	MinGPUMemGB    int     // minimum per-GPU memory
	MinDiskGB      int     // minimum disk space
	MinReliability float64 // minimum reliability score (0-1)
	NumGPUs        int     // number of GPUs needed (default 1)
}

// CreateOpts configures instance creation.
type CreateOpts struct {
	Image      string // Docker image
	DiskGB     int    // disk space to request
	SSHEnabled bool   // enable SSH access
	OnStartCmd string // command to run on instance start
}

// R2Config holds Cloudflare R2 credentials for instance-side uploads.
type R2Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
}

// ProgressFunc is called at each lifecycle phase to report status.
type ProgressFunc func(phase string)

// DefaultImage is the default Docker image for cloud instances.
const DefaultImage = "nvidia/cuda:12.2-devel-ubuntu22.04"

// DefaultWaitReadyTimeout is the default timeout for waiting for an instance.
const DefaultWaitReadyTimeout = 5 * time.Minute
