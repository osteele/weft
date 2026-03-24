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
		MinDiskGB:            constraints.MinDiskGB,
		MinReliability:       constraints.MinReliability,
		NumGPUs:              constraints.NumGPUs,
		ExcludeGeos:          constraints.ExcludeGeos,
		MinCPUCoresEffective: constraints.MinCPUCoresEffective,
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
		Image:      opts.Image,
		DiskGB:     opts.DiskGB,
		SSHEnabled: opts.SSHEnabled,
		OnStartCmd: opts.OnStartCmd,
		EnvVars:    opts.EnvVars,
		Label:      opts.Label,
	}
	inst, err := c.inner.CreateInstance(id, vopts)
	if err != nil {
		return nil, err
	}
	return instanceToCloud(inst), nil
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
	// Prefer Vast.ai's per-instance credentials (CONTAINER_ID + CONTAINER_API_KEY),
	// which are set in PID 1's environment and inherited by the onstart script.
	// Fall back to the user's API key if the env vars aren't available.
	// -f (--fail) makes curl return non-zero on HTTP errors so retries work.
	return fmt.Sprintf(
		`curl -sf -X DELETE "https://console.vast.ai/api/v0/instances/${CONTAINER_ID:-%s}/" `+
			`-H "Authorization: Bearer ${CONTAINER_API_KEY:-%s}"`,
		providerInstanceID, ReadAPIKey(),
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
	return cloud.Offer{
		ProviderID:        strconv.Itoa(o.ID),
		Provider:          cloud.ProviderVastai,
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
