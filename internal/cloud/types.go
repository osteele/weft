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

// DisplayName returns the human-readable provider name for UI output
// (e.g. "Vast.ai", "RunPod"). Returns "" for unknown providers so callers
// can omit the segment entirely.
func (p Provider) DisplayName() string {
	switch p {
	case ProviderVastai:
		return "Vast.ai"
	case ProviderRunpod:
		return "RunPod"
	default:
		return ""
	}
}

// ShortCode returns a 2-letter tag for compact UI contexts — jobs-list HOST
// column, grouped "launching" row suffix — where naming the provider
// explicitly would blow the column budget. Returns "" for unknown providers
// (callers should fall back to the bare instance id).
func (p Provider) ShortCode() string {
	switch p {
	case ProviderVastai:
		return "va"
	case ProviderRunpod:
		return "rp"
	default:
		return ""
	}
}

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
	CPUCores          int     // effective CPU cores granted
	CPUName           string  // CPU model name
	RAMGB             int     // total system RAM in GB
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
	ProviderStatusCreated  = "created"
	ProviderStatusCreating = "creating" // RunPod initial state
	ProviderStatusLoading  = "loading"
	ProviderStatusRunning  = "running"
	ProviderStatusExited   = "exited"
	ProviderStatusStopped  = "stopped"
	// ProviderStatusOffline is Vast.ai's status for an interruptible
	// instance that has been preempted but whose data is still preserved
	// and may resume when capacity returns. Treated as pause-tolerant for
	// interruptible launches; terminal otherwise.
	ProviderStatusOffline   = "offline"
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
	StatusMsg      string // free-form provider message accompanying Status (e.g., reason for offline/error)
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
	MaxGPUMemGB          int      // deprecated; ignored by placement
	MinDiskGB            int      // minimum disk space
	MinReliability       float64  // minimum reliability score (0-1)
	MinDriverVersion     int      // minimum NVIDIA driver major version (0 = no floor)
	MinCUDAVersion       string   // minimum provider CUDA runtime/driver compatibility (e.g. "12.8")
	NumGPUs              int      // number of GPUs needed (default 1)
	Interconnect         string   // required intra-host interconnect: any, pcie, nvlink
	ExcludeGeos          []string // two-letter country codes to exclude (e.g., ["CN"])
	MinCPUCoresEffective int      // minimum effective CPU cores (e.g., for compute-intensive jobs)
	InstanceType         string   // desired rental type ("on-demand" or "interruptible")
}

// DefaultExcludeGeos lists countries excluded by default from cloud offers.
// Empty by default; set to e.g. []string{"CN"} to exclude specific regions.
var DefaultExcludeGeos []string

// CreateOpts configures instance creation.
type CreateOpts struct {
	Image            string            // Docker image
	DiskGB           int               // disk space to request
	GPUCount         int               // number of GPUs to request (provider-specific; defaults to 1)
	SSHEnabled       bool              // enable SSH access
	SSHIdentityFile  string            // private key file for non-interactive SSH
	SSHPublicKeyFile string            // public key file to register/attach with providers
	OnStartCmd       string            // command to run on instance start
	EnvVars          map[string]string // environment variables passed via provider's env mechanism
	CapAdd           []string          // provider-specific Linux capabilities (currently used for Vast.ai --cap-add)
	TemplateID       string            // provider template ID for startup-managed images
	Label            string            // instance label/name visible in provider dashboard (e.g., "weft/c42")
	InstanceType     string            // desired rental type ("on-demand" or "interruptible"), when provider supports it
	MaxBidPrice      float64           // max bid/price for interruptible rentals, when provider supports it
	MinCUDAVersion   string            // minimum provider CUDA runtime/driver compatibility (e.g. "12.8")
	RegistryAuth     *RegistryAuth     // credentials for pulling private images
	RunpodRegistryID string            // resolved RunPod registry auth ID
}

// ImageRequirements describes Docker-image runtime constraints discovered from
// image metadata or declared explicitly by the user.
type ImageRequirements struct {
	MinDriverVersion int
	MinCUDAVersion   string
}

// RegistryAuth holds private Docker registry credentials.
type RegistryAuth struct {
	Host     string
	Username string
	Password string
	Name     string
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

// DefaultRunpodImage is the default Docker image for RunPod pods.
// It includes RunPod's init/SSH stack used by weft's SSH bootstrap path.
const DefaultRunpodImage = "runpod/base:1.0.2-ubuntu2204"

// Each step skips if already on PATH, else retries up to 3x with 10s
// backoff. Without retries a single DNS/apt hiccup forfeits the rental:
// the OnStart shell exits non-zero, Vast destroys the container, and
// weft only notices via the empty_status_timeout watchdog (~25min).
//
// Before installing anything, fire a best-effort probe to the presigned
// R2 URL in $WEFT_PROBE_URL. Its presence on R2 proves OnStart actually
// executed and had outbound network — when a launch dies with no other
// R2 markers, this discriminates "OnStart never ran" (provider/host
// issue) from "OnStart ran but rclone/apt failed" (retried below).
//
// rclone is the hard dependency (bootstrap stage markers depend on it).
// The package list must stay in sync with deploy/cloud-base.Dockerfile,
// which pre-bakes the same dependencies so the `command -v` checks
// short-circuit.
const DefaultOnStartCmd = `set +e
_weft_stage() {
  [ -z "${WEFT_STAGE_URL:-}" ] && return 0
  body="$1 $(date -u +%FT%TZ)"
  (command -v curl >/dev/null && curl -fsS -m 10 -X PUT --data "$body" "${WEFT_STAGE_URL}" >/dev/null 2>&1) || \
    (command -v wget >/dev/null && wget -q --method=PUT --body-data="$body" -O /dev/null "${WEFT_STAGE_URL}") || true
}
if [ -n "${WEFT_PROBE_URL:-}" ]; then
  probe_body="onstart-started $(date -u +%FT%TZ)"
  (command -v curl >/dev/null && curl -fsS -m 10 -X PUT --data "$probe_body" "${WEFT_PROBE_URL}" >/dev/null 2>&1) || \
    (command -v wget >/dev/null && wget -q --method=PUT --body-data="$probe_body" -O /dev/null "${WEFT_PROBE_URL}") || true
fi
_weft_stage onstart-started
if ! (command -v unzip >/dev/null && command -v gcc >/dev/null && command -v curl >/dev/null); then
  _weft_stage apt-installing
  for i in 1 2 3; do apt-get update -qq && apt-get install -y -qq unzip gcc g++ python3-dev curl && break; sleep 10; done
  command -v curl >/dev/null && _weft_stage apt-ok || _weft_stage apt-failed
else
  _weft_stage apt-skipped
fi
if ! command -v uv >/dev/null; then
  _weft_stage uv-installing
  for i in 1 2 3; do curl -fsSL https://astral.sh/uv/install.sh | sh && break; sleep 10; done
  command -v uv >/dev/null && _weft_stage uv-ok || _weft_stage uv-failed
else
  _weft_stage uv-skipped
fi
if ! command -v rclone >/dev/null; then
  _weft_stage rclone-installing
  for i in 1 2 3; do curl -fsSL https://rclone.org/install.sh | bash && break; sleep 10; done
  command -v rclone >/dev/null && _weft_stage rclone-ok || _weft_stage rclone-failed
else
  _weft_stage rclone-skipped
fi
command -v rclone >/dev/null || { _weft_stage rclone-missing-exit; echo 'rclone install failed after retries' >&2; exit 1; }
_weft_stage onstart-deps-ready`

// R2BootstrapKeyEnvVar is the env var a template-managed startup command reads
// to locate the instance-specific bootstrap script in R2.
const R2BootstrapKeyEnvVar = "WEFT_BOOTSTRAP_KEY"

// OnStartProbeURLEnvVar carries a presigned PUT URL that OnStart hits
// before any setup, to prove the container actually ran OnStart with
// outbound network. The literal name is also embedded in DefaultOnStartCmd.
const OnStartProbeURLEnvVar = "WEFT_PROBE_URL"

// OnStartStageURLEnvVar carries a presigned PUT URL that the OnStart shell
// overwrites at each major step (apt, uv, rclone, deps-ready). The R2
// object becomes a "last successful step" marker: when the chain dies
// silently, R2 shows the last stage that ran instead of just nothing.
// Each successful step writes a new last-write-wins value, so the surviving
// content names the boundary where OnStart stopped.
const OnStartStageURLEnvVar = "WEFT_STAGE_URL"

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
rclone_ready=0
for i in $(seq 1 60); do
  if rclone cat "r2:${R2_BUCKET}/%s" > /tmp/bootstrap.sh; then
    rclone_ready=1
    break
  fi
  sleep 5
done
[ "$rclone_ready" = 1 ] || { _weft_stage bootstrap-missing-exit; echo 'bootstrap script not available after retries' >&2; exit 1; }
bash /tmp/bootstrap.sh`,
		DefaultOnStartCmd, bootstrapKey,
	)
}

// R2BootstrapTemplateStartCmd returns a startup command suitable for a
// provider template. It expects the bootstrap key in $WEFT_BOOTSTRAP_KEY.
func R2BootstrapTemplateStartCmd() string {
	return R2BootstrapOnStartCmd("${" + R2BootstrapKeyEnvVar + "}")
}
