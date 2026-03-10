package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

// cliTimeout is the maximum time to wait for a runpodctl CLI command to complete.
const cliTimeout = 30 * time.Second

// CloudClient implements cloud.Client via the runpodctl CLI.
type CloudClient struct {
	cliPath string
}

var _ cloud.Client = (*CloudClient)(nil)

// NewCloudClient creates a CloudClient that uses runpodctl from PATH.
func NewCloudClient() *CloudClient {
	return &CloudClient{cliPath: "runpodctl"}
}

func (c *CloudClient) Provider() cloud.Provider {
	return cloud.ProviderRunpod
}

func (c *CloudClient) Available() error {
	path, err := exec.LookPath(c.cliPath)
	if err != nil {
		return fmt.Errorf("runpodctl not found in PATH (install: https://docs.runpod.io/cli/install)")
	}
	// Quick check: runpodctl version
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("runpodctl check failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (c *CloudClient) SearchOffers(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	// runpodctl get gpu lists available GPU types
	out, err := c.run("get", "gpu")
	if err != nil {
		return nil, fmt.Errorf("list GPU types: %w", err)
	}

	offers, err := parseGPUTypeOutput(out, constraints)
	if err != nil {
		return nil, err
	}

	return offers, nil
}

func (c *CloudClient) CreateInstance(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
	args := []string{"create", "pod",
		"--gpuType", offerID,
		"--gpuCount", "1",
	}
	if opts.Image != "" {
		args = append(args, "--imageName", opts.Image)
	}
	if opts.DiskGB > 0 {
		args = append(args, "--volumeSize", fmt.Sprintf("%d", opts.DiskGB))
	}
	if opts.Label != "" {
		args = append(args, "--name", opts.Label)
	}

	out, err := c.run(args...)
	if err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}

	// Parse pod ID from output
	podID := strings.TrimSpace(string(out))
	return &cloud.Instance{
		ProviderID: podID,
		Provider:   cloud.ProviderRunpod,
		Status:     "creating",
	}, nil
}

func (c *CloudClient) ListAllInstances() ([]cloud.Instance, error) {
	out, err := c.run("get", "pod")
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	var pods []Pod
	if err := json.Unmarshal(out, &pods); err != nil {
		return nil, fmt.Errorf("parse pods: %w", err)
	}

	result := make([]cloud.Instance, len(pods))
	for i, pod := range pods {
		result[i] = cloud.Instance{
			ProviderID:  pod.ID,
			Provider:    cloud.ProviderRunpod,
			Status:      strings.ToLower(pod.Status),
			SSHHost:     pod.SSHHost,
			SSHPort:     pod.SSHPort,
			CostPerHour: pod.CostPerHour,
			Label:       pod.Name,
		}
	}
	return result, nil
}

func (c *CloudClient) ShowInstance(instanceID string) (*cloud.Instance, error) {
	out, err := c.run("get", "pod", instanceID)
	if err != nil {
		return nil, fmt.Errorf("get pod %s: %w", instanceID, err)
	}

	var pod Pod
	if err := json.Unmarshal(out, &pod); err != nil {
		return nil, fmt.Errorf("parse pod response: %w", err)
	}

	status := strings.ToLower(pod.Status)

	return &cloud.Instance{
		ProviderID:  pod.ID,
		Provider:    cloud.ProviderRunpod,
		Status:      status,
		SSHHost:     pod.SSHHost,
		SSHPort:     pod.SSHPort,
		CostPerHour: pod.CostPerHour,
	}, nil
}

func (c *CloudClient) WaitReady(instanceID string, timeout time.Duration) (*cloud.Instance, error) {
	deadline := time.Now().Add(timeout)
	poll := 5 * time.Second

	for time.Now().Before(deadline) {
		inst, err := c.ShowInstance(instanceID)
		if err != nil {
			time.Sleep(poll)
			continue
		}
		if inst.Status == "running" {
			return inst, nil
		}
		if inst.Status == "exited" || inst.Status == "error" {
			return inst, fmt.Errorf("pod %s entered state %q", instanceID, inst.Status)
		}
		time.Sleep(poll)
	}
	return nil, fmt.Errorf("pod %s not ready after %v", instanceID, timeout)
}

func (c *CloudClient) DestroyInstance(instanceID string) error {
	_, err := c.run("remove", "pod", instanceID)
	if err != nil {
		return fmt.Errorf("remove pod %s: %w", instanceID, err)
	}
	return nil
}

func (c *CloudClient) CopyBetweenInstances(_, _ string, _, _ string) error {
	return fmt.Errorf("RunPod does not support inter-instance copy")
}

func (c *CloudClient) WorkspacePath() string {
	return "/workspace/"
}

func (c *CloudClient) SelfDestructCmd(providerInstanceID string) string {
	// RunPod sets $RUNPOD_POD_ID in the container, and runpodctl is pre-installed.
	return `runpodctl remove pod "$RUNPOD_POD_ID" 2>/dev/null || true`
}

func (c *CloudClient) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// parseGPUTypeOutput parses runpodctl get gpu output and filters by constraints.
func parseGPUTypeOutput(data []byte, constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	var gpuTypes []GPUType
	if err := json.Unmarshal(data, &gpuTypes); err != nil {
		return nil, fmt.Errorf("parse GPU types: %w", err)
	}

	var offers []cloud.Offer
	for _, gt := range gpuTypes {
		// Filter by memory
		if constraints.MinGPUMemGB > 0 && gt.MemoryInGB < constraints.MinGPUMemGB {
			continue
		}

		// Get best price (prefer community cloud for lower cost)
		price := gt.CommunityPrice
		if price <= 0 {
			price = gt.SecurePrice
		}
		if price <= 0 && gt.LowestPrice != nil {
			price = gt.LowestPrice.Uninterruptable
		}
		if price <= 0 {
			continue
		}

		offers = append(offers, cloud.Offer{
			ProviderID:  gt.ID,
			Provider:    cloud.ProviderRunpod,
			GPUName:     gt.DisplayName,
			NumGPUs:     1,
			GPUMemGB:    float64(gt.MemoryInGB),
			CostPerHour: price,
			Verified:    gt.SecureCloud,
		})
	}

	return offers, nil
}
