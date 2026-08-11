// Package vastai wraps the vastai CLI for searching GPU offers,
// creating instances, and managing their lifecycle.
package vastai

import (
	"encoding/json"

	"github.com/osteele/weft/internal/cloud"
)

// Offer represents a Vast.ai GPU rental offer from search results.
type Offer struct {
	ID                int      `json:"id"`
	InstanceType      string   `json:"type"`
	GPUName           string   `json:"gpu_name"`
	NumGPUs           int      `json:"num_gpus"`
	GPUMemMB          int      `json:"gpu_ram"` // per GPU, in MB
	GPUMemGB          float64  // computed: GPUMemMB / 1024
	CostPerHour       float64  `json:"dph_total"`    // $/hr for the whole instance
	Reliability       float64  `json:"reliability2"` // 0-1
	DownloadBandwidth float64  `json:"inet_down"`    // Mbps
	UploadBandwidth   float64  `json:"inet_up"`      // Mbps
	NVLinkBandwidth   *float64 `json:"bw_nvlink"`    // GB/s measured NVLink bandwidth; nil = not reported
	DiskSpace         float64  `json:"disk_space"`   // GB available
	CPUCores          float64  `json:"cpu_cores_effective"`
	CPUName           string   `json:"cpu_name"`
	CPURAMMB          float64  `json:"cpu_ram"`        // total system RAM in MB
	CUDAVersion       float64  `json:"cuda_max_good"`  // max supported CUDA version
	DriverVersion     string   `json:"driver_version"` // dotted NVIDIA driver version ("550.90.07")
	DatacenterDriver  bool     `json:"datacenter"`     // Vast.ai datacenter/forward-compat driver stack
	DLPerf            float64  `json:"dlperf"`         // deep learning perf score
	Geolocation       string   `json:"geolocation"`    // data center location
	Verified          bool     `json:"verified"`
	MachineID         int      `json:"machine_id"`
}

// Instance represents a running Vast.ai instance.
type Instance struct {
	ID             int     `json:"id"`
	Status         string  `json:"actual_status"` // "running", "loading", "exited", etc.
	SSHHost        string  `json:"ssh_host"`
	SSHPort        int     `json:"ssh_port"`
	CostPerHour    float64 `json:"dph_total"`
	DiskSpace      float64 `json:"disk_space"`
	Label          string  `json:"label"`
	CPUCores       float64 `json:"cpu_cores_effective"`
	CPUName        string  `json:"cpu_name"`
	CPURAMMB       float64 `json:"cpu_ram"` // total system RAM in MB
	MachineID      int     `json:"machine_id"`
	StatusMsg      string  `json:"status_msg"`
	IntendedStatus string  `json:"intended_status"`
}

// BidUpdate records a successful bid-change response from Vast.ai.
type BidUpdate struct {
	Success bool            `json:"success"`
	Error   json.RawMessage `json:"error"`
	Msg     string          `json:"msg"`
}

// User represents Vast.ai account information.
type User struct {
	Credit float64 `json:"credit"`
	Email  string  `json:"email"`
	CanPay bool    `json:"can_pay"`
}

// OfferConstraints describes what GPU capabilities a job needs.
type OfferConstraints struct {
	GPUClass             string   // e.g., "RTX_4090", "A100" (mapped to Vast.ai gpu_name)
	MinGPUMemGB          int      // minimum per-GPU memory
	MaxGPUMemGB          int      // deprecated; ignored by placement
	MinDiskGB            int      // minimum disk space
	MinReliability       float64  // minimum reliability score (0-1)
	MinDriverVersion     int      // minimum NVIDIA driver major version
	MinCUDAVersion       string   // minimum CUDA compatibility version
	NumGPUs              int      // number of GPUs needed (default 1)
	ExcludeGeos          []string // two-letter country codes to exclude (e.g., ["CN"])
	MinCPUCoresEffective int      // minimum effective CPU cores
	MinHostRAMGB         int      // minimum host/system RAM in GB (Vast.ai cpu_ram, reported in MB)
	InstanceType         string   // "on-demand" or "interruptible"
}

// DefaultImage is the default Docker image for Vast.ai instances.
const DefaultImage = "nvidia/cuda:12.4.1-runtime-ubuntu22.04"

// CreateOpts configures instance creation.
type CreateOpts struct {
	Image         string            // Docker image (e.g., "nvidia/cuda:12.2-devel-ubuntu22.04")
	DiskGB        int               // disk space to request
	SSHEnabled    bool              // enable SSH access
	OnStartCmd    string            // command to run on instance start
	EnvVars       map[string]string // environment variables passed via --env flag
	CapAdd        []string          // extra Linux capabilities passed via --cap-add
	Label         string            // instance label visible in Vast.ai dashboard
	InstanceType  string            // "on-demand" or "interruptible"
	MaxBidPrice   float64           // max bid/price for interruptible instances
	RegistryAuth  *cloud.RegistryAuth
	PublicKeyFile string // SSH public key to attach to the created instance
}
