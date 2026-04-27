package vastai

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

// CloudClient adapts the Vast.ai VastaiClient to the cloud.Client interface.
type CloudClient struct {
	inner VastaiClient
}

var _ cloud.Client = (*CloudClient)(nil)

// NewCloudClient wraps a VastaiClient as a cloud.Client.
func NewCloudClient(inner VastaiClient) *CloudClient {
	return &CloudClient{inner: inner}
}

func (c *CloudClient) Provider() cloud.Provider {
	return cloud.ProviderVastai
}

func (c *CloudClient) Available() error {
	return c.inner.Available()
}

func (c *CloudClient) SearchOffers(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	vc := OfferConstraints{
		GPUClass:             constraints.GPUClass,
		MinGPUMemGB:          constraints.MinGPUMemGB,
		MaxGPUMemGB:          constraints.MaxGPUMemGB,
		MinDiskGB:            constraints.MinDiskGB,
		MinReliability:       constraints.MinReliability,
		NumGPUs:              constraints.NumGPUs,
		ExcludeGeos:          constraints.ExcludeGeos,
		MinCPUCoresEffective: constraints.MinCPUCoresEffective,
		InstanceType:         constraints.InstanceType,
	}
	offers, err := c.inner.SearchOffers(vc)
	if err != nil {
		return nil, err
	}
	result := make([]cloud.Offer, len(offers))
	for i, o := range offers {
		result[i] = offerToCloud(o)
	}
	return result, nil
}

func (c *CloudClient) CreateInstance(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
	id, err := strconv.Atoi(offerID)
	if err != nil {
		return nil, fmt.Errorf("parse vastai offer ID %q: %w", offerID, err)
	}
	vopts := CreateOpts{
		Image:        opts.Image,
		DiskGB:       opts.DiskGB,
		SSHEnabled:   opts.SSHEnabled,
		OnStartCmd:   opts.OnStartCmd,
		EnvVars:      opts.EnvVars,
		CapAdd:       opts.CapAdd,
		Label:        opts.Label,
		InstanceType: opts.InstanceType,
		MaxBidPrice:  opts.MaxBidPrice,
	}
	inst, err := c.inner.CreateInstance(id, vopts)
	if err != nil {
		return nil, err
	}
	return instanceToCloud(inst), nil
}

func (c *CloudClient) CreateInstanceWithProgress(offerID string, opts cloud.CreateOpts, progress cloud.ProgressFunc) (*cloud.Instance, error) {
	if progress == nil {
		progress = func(string) {}
	}
	progress("vastai: submitting create request")
	inst, err := c.CreateInstance(offerID, opts)
	if err != nil {
		progress("vastai: create request failed")
		return nil, err
	}
	progress("vastai: create request accepted")
	return inst, nil
}

func (c *CloudClient) ListAllInstances() ([]cloud.Instance, error) {
	instances, err := c.inner.ListAllInstances()
	if err != nil {
		return nil, err
	}
	result := make([]cloud.Instance, len(instances))
	for i, inst := range instances {
		result[i] = *instanceToCloud(&inst)
	}
	return result, nil
}

func (c *CloudClient) ShowInstance(instanceID string) (*cloud.Instance, error) {
	id, err := strconv.Atoi(instanceID)
	if err != nil {
		return nil, fmt.Errorf("parse vastai instance ID %q: %w", instanceID, err)
	}
	inst, err := c.inner.ShowInstance(id)
	if err != nil {
		return nil, err
	}
	return instanceToCloud(inst), nil
}

func (c *CloudClient) WaitReady(instanceID string, timeout time.Duration) (*cloud.Instance, error) {
	id, err := strconv.Atoi(instanceID)
	if err != nil {
		return nil, fmt.Errorf("parse vastai instance ID %q: %w", instanceID, err)
	}
	inst, err := c.inner.WaitReady(id, timeout)
	if err != nil {
		return nil, err
	}
	return instanceToCloud(inst), nil
}

func (c *CloudClient) DestroyInstance(instanceID string) error {
	id, err := strconv.Atoi(instanceID)
	if err != nil {
		return fmt.Errorf("parse vastai instance ID %q: %w", instanceID, err)
	}
	return c.inner.DestroyInstance(id)
}

func (c *CloudClient) CopyBetweenInstances(srcInstanceID, srcPath, dstInstanceID, dstPath string) error {
	srcID, err := strconv.Atoi(srcInstanceID)
	if err != nil {
		return fmt.Errorf("parse vastai src instance ID %q: %w", srcInstanceID, err)
	}
	dstID, err := strconv.Atoi(dstInstanceID)
	if err != nil {
		return fmt.Errorf("parse vastai dst instance ID %q: %w", dstInstanceID, err)
	}
	return c.inner.CopyBetweenInstances(srcID, srcPath, dstID, dstPath)
}

func (c *CloudClient) SelfDestructCmd(providerInstanceID string) string {
	return c.selfDestructCmdWithKey(providerInstanceID, ReadAPIKey())
}

// selfDestructCmdWithKey tries the per-instance Vast credentials first and
// falls back to the user key on *any* failure (auth, network, missing var) —
// not just when CONTAINER_API_KEY is unset. On HTTP failure the helper writes
// "HTTP=<code> url=<url> body=<body>" to stderr so termination-intent.json's
// LastError distinguishes 401/403/404/5xx instead of all looking like
// "exit status 22".
func (c *CloudClient) selfDestructCmdWithKey(providerInstanceID, userKey string) string {
	return fmt.Sprintf(
		`_d() { `+
			`local url=$1 key=$2 body code; body=$(mktemp); `+
			`code=$(curl -sS -o "$body" -w '%%{http_code}' -X DELETE "$url" `+
			`-H "Authorization: Bearer $key" 2>>"$body"); `+
			`case "$code" in `+
			`2*) rm -f "$body"; return 0;; `+
			`*) printf 'HTTP=%%s url=%%s body=%%s\n' "$code" "$url" `+
			`"$(tr -d "\n" < "$body" | head -c 500)" >&2; rm -f "$body"; return 22;; `+
			`esac; `+
			`}; `+
			`{ [ -n "${CONTAINER_API_KEY:-}" ] && `+
			`_d "https://console.vast.ai/api/v0/instances/${CONTAINER_ID:-%s}/" "$CONTAINER_API_KEY"; } || `+
			`_d "https://console.vast.ai/api/v0/instances/%s/" "%s"`,
		providerInstanceID, providerInstanceID, userKey,
	)
}

// ReadAPIKey reads the API key from the vastai config file.
// Checks ~/.config/vastai/vast_api_key first, then ~/.vast_api_key (legacy).
func ReadAPIKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, path := range []string{
		filepath.Join(home, ".config", "vastai", "vast_api_key"),
		filepath.Join(home, ".vast_api_key"),
	} {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// Inner returns the underlying VastaiClient for Vast.ai-specific operations.
func (c *CloudClient) Inner() VastaiClient {
	return c.inner
}

// machineIDToString converts a Vast.ai machine ID (int) to the cloud-layer
// string representation. Zero means unknown/unset and maps to empty string.
func machineIDToString(id int) string {
	if id == 0 {
		return ""
	}
	return strconv.Itoa(id)
}

func offerToCloud(o Offer) cloud.Offer {
	instanceType := o.InstanceType
	if instanceType == "" {
		instanceType = cloud.InstanceTypeOnDemand
	}
	return cloud.Offer{
		ProviderID:        strconv.Itoa(o.ID),
		Provider:          cloud.ProviderVastai,
		InstanceType:      instanceType,
		GPUName:           o.GPUName,
		NumGPUs:           o.NumGPUs,
		GPUMemGB:          o.GPUMemGB,
		CostPerHour:       o.CostPerHour,
		Reliability:       o.Reliability,
		DLPerf:            o.DLPerf,
		DataCenter:        o.Geolocation,
		CUDAVersion:       o.CUDAVersion,
		DiskSpaceGB:       o.DiskSpace,
		DownloadBandwidth: o.DownloadBandwidth,
		UploadBandwidth:   o.UploadBandwidth,
		Verified:          o.Verified,
		MachineID:         machineIDToString(o.MachineID),
	}
}

func instanceToCloud(inst *Instance) *cloud.Instance {
	if inst == nil {
		return nil
	}
	return &cloud.Instance{
		ProviderID:     strconv.Itoa(inst.ID),
		Provider:       cloud.ProviderVastai,
		Status:         inst.Status,
		IntendedStatus: inst.IntendedStatus,
		SSHHost:        inst.SSHHost,
		SSHPort:        inst.SSHPort,
		CostPerHour:    inst.CostPerHour,
		DiskGB:         inst.DiskSpace,
		Label:          inst.Label,
		CPUCores:       int(inst.CPUCores),
		CPUName:        inst.CPUName,
		RAMGB:          (int(inst.CPURAMMB) + 512) / 1024,
		MachineID:      machineIDToString(inst.MachineID),
	}
}
