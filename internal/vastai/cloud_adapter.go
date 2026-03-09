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
		GPUClass:       constraints.GPUClass,
		MinGPUMemGB:    constraints.MinGPUMemGB,
		MinDiskGB:      constraints.MinDiskGB,
		MinReliability: constraints.MinReliability,
		NumGPUs:        constraints.NumGPUs,
		ExcludeGeos:    constraints.ExcludeGeos,
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
	}
	inst, err := c.inner.CreateInstance(id, vopts)
	if err != nil {
		return nil, err
	}
	return instanceToCloud(inst), nil
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

func (c *CloudClient) WorkspacePath() string {
	return "/workspace/"
}

func (c *CloudClient) SelfDestructCmd(providerInstanceID string) string {
	apiKey := ReadAPIKey()
	if apiKey == "" {
		return fmt.Sprintf("echo 'warning: no vastai API key found, cannot self-destruct instance %s'", providerInstanceID)
	}
	// Use the REST API directly — the vastai CLI is not installed on instances.
	// -f (--fail) makes curl return non-zero on HTTP errors so retries work.
	return fmt.Sprintf(
		`curl -sf -X DELETE "https://console.vast.ai/api/v0/instances/%s/" -H "Authorization: Bearer %s"`,
		providerInstanceID, apiKey,
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
	}
}

func instanceToCloud(inst *Instance) *cloud.Instance {
	if inst == nil {
		return nil
	}
	return &cloud.Instance{
		ProviderID:  strconv.Itoa(inst.ID),
		Provider:    cloud.ProviderVastai,
		Status:      inst.Status,
		SSHHost:     inst.SSHHost,
		SSHPort:     inst.SSHPort,
		CostPerHour: inst.CostPerHour,
	}
}
