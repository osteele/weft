// Package cloud provides provider-neutral types and interfaces for cloud GPU providers.
package cloud

import "time"

// Provider identifies a cloud GPU provider.
type Provider string

const (
	// DefaultMinReliability is the minimum reliability score for cloud offers.
	DefaultMinReliability = 0.98

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
const DefaultImage = "nvidia/cuda:12.4.1-runtime-ubuntu22.04"

// DefaultOnStartCmd installs dependencies, uv, and rclone on fresh instances.
// The runtime CUDA images lack unzip (needed by rclone installer) and build tools.
const DefaultOnStartCmd = "apt-get update -qq && apt-get install -y -qq unzip gcc g++ python3-dev && curl -LsSf https://astral.sh/uv/install.sh | sh && curl https://rclone.org/install.sh | bash"

// DefaultCreateOpts returns standard instance creation options.
// If image is empty, DefaultImage is used.
func DefaultCreateOpts(image string) CreateOpts {
	if image == "" {
		image = DefaultImage
	}
	return CreateOpts{
		Image:      image,
		DiskGB:     50,
		SSHEnabled: true,
		OnStartCmd: DefaultOnStartCmd,
	}
}

// DefaultWaitReadyTimeout is the default timeout for waiting for an instance.
const DefaultWaitReadyTimeout = 5 * time.Minute
