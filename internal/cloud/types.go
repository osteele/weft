// Package cloud provides provider-neutral types and interfaces for cloud GPU providers.
package cloud

import (
	"fmt"
	"time"
)

// Provider identifies a cloud GPU provider.
type Provider string

const (
	// DefaultMinReliability is the minimum reliability score for cloud offers.
	DefaultMinReliability = 0.95

	ProviderVastai Provider = "vastai"
	ProviderRunpod Provider = "runpod"
)

// InstanceType describes cloud rental interruption behavior.
const (
	InstanceTypeOnDemand      = "on-demand"
	InstanceTypeInterruptible = "interruptible"
)

// Offer represents a GPU rental offer from any cloud provider.
type Offer struct {
	ProviderID        string   // provider-specific offer ID
	Provider          Provider // which provider
	InstanceType      string   // interruption mode (on-demand, interruptible), when known
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
	MachineID         string // provider-specific physical machine identifier
}

// Key returns a provider-qualified identifier for the offer (e.g. "vastai:12345").
func (o Offer) Key() string {
	return string(o.Provider) + ":" + o.ProviderID
}

// ProviderStatus constants represent provider-reported instance states,
// normalized across providers (Vast.ai, RunPod).
const (
	ProviderStatusCreated   = "created"
	ProviderStatusCreating  = "creating" // RunPod initial state
	ProviderStatusLoading   = "loading"
	ProviderStatusRunning   = "running"
	ProviderStatusExited    = "exited"
	ProviderStatusStopped   = "stopped"
	ProviderStatusError     = "error"
	ProviderStatusDestroyed = "destroyed"
	ProviderStatusDead      = "dead"
)

// Instance represents a running cloud instance from any provider.
type Instance struct {
	ProviderID     string // provider-specific instance ID
	Provider       Provider
	Status         string // "running", "loading", "exited", etc.
	IntendedStatus string // provider's intended/target status (e.g., "running", "stopped")
	SSHHost        string
	SSHPort        int
	CostPerHour    float64
	DiskGB         float64
	DataCenter     string
	Label          string // provider-assigned label/name (e.g., "weft/c42")
	CPUCores       int    // effective CPU cores granted
	CPUName        string // CPU model name
	RAMGB          int    // total system RAM in GB
	MachineID      string // provider-specific physical machine identifier
}

// OfferConstraints describes what GPU capabilities a job needs.
type OfferConstraints struct {
	GPUClass             string   // e.g., "RTX_4090", "A100"
	MinGPUMemGB          int      // minimum per-GPU memory
	MaxGPUMemGB          int      // maximum per-GPU memory (0 = no ceiling)
	MinDiskGB            int      // minimum disk space
	MinReliability       float64  // minimum reliability score (0-1)
	NumGPUs              int      // number of GPUs needed (default 1)
	ExcludeGeos          []string // two-letter country codes to exclude (e.g., ["CN"])
	MinCPUCoresEffective int      // minimum effective CPU cores (e.g., for compute-intensive jobs)
	InstanceType         string   // desired rental type ("on-demand" or "interruptible")
}

// DefaultExcludeGeos lists countries excluded by default from cloud offers.
// Empty by default; set to e.g. []string{"CN"} to exclude specific regions.
var DefaultExcludeGeos []string

// CreateOpts configures instance creation.
type CreateOpts struct {
	Image        string            // Docker image
	DiskGB       int               // disk space to request
	GPUCount     int               // number of GPUs to request (provider-specific; defaults to 1)
	SSHEnabled   bool              // enable SSH access
	OnStartCmd   string            // command to run on instance start
	EnvVars      map[string]string // environment variables passed via provider's env mechanism
	CapAdd       []string          // provider-specific Linux capabilities (currently used for Vast.ai --cap-add)
	TemplateID   string            // provider template ID for startup-managed images
	Label        string            // instance label/name visible in provider dashboard (e.g., "weft/c42")
	InstanceType string            // desired rental type ("on-demand" or "interruptible"), when provider supports it
	MaxBidPrice  float64           // max bid/price for interruptible rentals, when provider supports it
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

// R2BootstrapKeyEnvVar is the env var a template-managed startup command reads
// to locate the instance-specific bootstrap script in R2.
const R2BootstrapKeyEnvVar = "WEFT_BOOTSTRAP_KEY"

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

// MbpsToBytesPerSec converts megabits per second to bytes per second.
func MbpsToBytesPerSec(mbps float64) float64 {
	return mbps * 1e6 / 8
}

// DefaultWaitReadyTimeout is the default timeout for waiting for an instance.
const DefaultWaitReadyTimeout = 5 * time.Minute

// R2BootstrapOnStartCmd returns a shell command for --onstart-cmd that:
// 1. Installs system deps, uv, and rclone (same as DefaultOnStartCmd)
// 2. Writes rclone config from R2_* env vars
// 3. Downloads and executes a bootstrap script from R2
func R2BootstrapOnStartCmd(bootstrapKey string) string {
	return fmt.Sprintf(
		`%s && mkdir -p ~/.config/rclone && cat > ~/.config/rclone/rclone.conf << RCLONE_EOF
[r2]
type = s3
provider = Cloudflare
access_key_id = ${R2_ACCESS_KEY_ID}
secret_access_key = ${R2_SECRET_ACCESS_KEY}
endpoint = ${R2_ENDPOINT}
RCLONE_EOF
rclone cat "r2:${R2_BUCKET}/%s" > /tmp/bootstrap.sh && bash /tmp/bootstrap.sh`,
		DefaultOnStartCmd, bootstrapKey,
	)
}

// R2BootstrapTemplateStartCmd returns a startup command suitable for a
// provider template. It expects the bootstrap key in $WEFT_BOOTSTRAP_KEY.
func R2BootstrapTemplateStartCmd() string {
	return R2BootstrapOnStartCmd("${" + R2BootstrapKeyEnvVar + "}")
}
