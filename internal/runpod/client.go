package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
)

// ErrPerPodStartupUnsupported is returned when a per-pod startup command is
// specified without a template.
var ErrPerPodStartupUnsupported = errors.New("runpod pods do not support per-pod startup commands")

// cliTimeout is the maximum time to wait for a runpodctl CLI command to complete.
const cliTimeout = 30 * time.Second

// CloudClient implements cloud.Client via the runpodctl CLI.
type CloudClient struct {
	cliPath string
	runner  *cliRunner
}

var _ cloud.Client = (*CloudClient)(nil)

// NewCloudClient creates a CloudClient that uses runpodctl from PATH.
func NewCloudClient() *CloudClient {
	cliPath := "runpodctl"
	return &CloudClient{cliPath: cliPath, runner: newCLIRunner(cliPath)}
}

func newCloudClientForTests(runner *cliRunner) *CloudClient {
	return &CloudClient{cliPath: runner.cliPath, runner: runner}
}

func (c *CloudClient) Provider() cloud.Provider {
	return cloud.ProviderRunpod
}

func (c *CloudClient) Available() error {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return err
	}
	if err := c.checkAuth(ctx, caps); err != nil {
		return err
	}
	return nil
}

func (c *CloudClient) SearchOffers(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	out, err := c.runner.runOutput(ctx, caps.path, caps.searchCommand...)
	if err != nil {
		return nil, fmt.Errorf("search offers: %w", err)
	}
	return parseSearchOutput(out, constraints)
}

func (c *CloudClient) CreateInstance(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
	args, err := buildCreatePodArgs(offerID, opts)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	out, err := c.runner.runOutput(ctx, caps.path, args...)
	if err != nil {
		if isOfferUnavailableError(err) {
			return nil, fmt.Errorf("%w: %v", cloud.ErrOfferUnavailable, err)
		}
		return nil, fmt.Errorf("create pod: %w", err)
	}

	podID, err := parseCreatedResourceID(out)
	if err != nil {
		return nil, err
	}
	return &cloud.Instance{
		ProviderID: podID,
		Provider:   cloud.ProviderRunpod,
		Status:     cloud.ProviderStatusCreating,
	}, nil
}

func podToInstance(pod Pod) cloud.Instance {
	return cloud.Instance{
		ProviderID:  pod.ID,
		Provider:    cloud.ProviderRunpod,
		Status:      strings.ToLower(pod.Status),
		SSHHost:     pod.SSHHost,
		SSHPort:     pod.SSHPort,
		CostPerHour: pod.CostPerHour,
		Label:       pod.Name,
	}
}

func (c *CloudClient) ListAllInstances() ([]cloud.Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	out, err := c.runner.runOutput(ctx, caps.path, caps.podListCommand...)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	pods, err := parsePods(out)
	if err != nil {
		return nil, fmt.Errorf("parse pods: %w", err)
	}
	result := make([]cloud.Instance, len(pods))
	for i, pod := range pods {
		result[i] = podToInstance(pod)
	}
	return result, nil
}

func (c *CloudClient) ShowInstance(instanceID string) (*cloud.Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	args := append(append([]string{}, caps.podGetCommand...), instanceID)
	out, err := c.runner.runOutput(ctx, caps.path, args...)
	if err != nil {
		return nil, fmt.Errorf("get pod %s: %w", instanceID, err)
	}

	pod, err := parsePod(out)
	if err != nil {
		return nil, fmt.Errorf("parse pod response: %w", err)
	}
	inst := podToInstance(*pod)
	return &inst, nil
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
		if inst.Status == cloud.ProviderStatusRunning {
			return inst, nil
		}
		if inst.Status == cloud.ProviderStatusExited || inst.Status == cloud.ProviderStatusError {
			return inst, fmt.Errorf("pod %s entered state %q", instanceID, inst.Status)
		}
		time.Sleep(poll)
	}
	return nil, fmt.Errorf("pod %s not ready after %v", instanceID, timeout)
}

func (c *CloudClient) DestroyInstance(instanceID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return err
	}
	args := append(append([]string{}, caps.podDeleteCommand...), instanceID)
	if _, err := c.runner.runOutput(ctx, caps.path, args...); err != nil {
		return fmt.Errorf("remove pod %s: %w", instanceID, err)
	}
	return nil
}

func (c *CloudClient) CopyBetweenInstances(_, _ string, _, _ string) error {
	return fmt.Errorf("RunPod does not support inter-instance copy")
}

func (c *CloudClient) SelfDestructCmd(providerInstanceID string) string {
	return `runpodctl pod delete "${RUNPOD_POD_ID:-` + providerInstanceID + `}" 2>/dev/null || true`
}

func (c *CloudClient) capabilities(ctx context.Context) (*cliCapabilities, error) {
	return c.runner.detectCapabilities(ctx)
}

func (c *CloudClient) checkAuth(ctx context.Context, caps *cliCapabilities) error {
	if _, err := c.runner.runOutput(ctx, caps.path, "user"); err != nil {
		return fmt.Errorf("runpodctl auth check failed: %w", err)
	}
	return nil
}

func (c *CloudClient) listTemplates(ctx context.Context, caps *cliCapabilities) ([]*TemplateInfo, error) {
	if len(caps.templateListCommand) == 0 || len(caps.templateGetCommand) == 0 {
		return nil, fmt.Errorf("runpodctl template commands are unavailable")
	}
	out, err := c.runner.runOutput(ctx, caps.path, caps.templateListCommand...)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	items, err := decodeJSONArray(out)
	if err != nil {
		return nil, fmt.Errorf("parse template list: %w", err)
	}
	templates := make([]*TemplateInfo, 0, len(items))
	for _, item := range items {
		id := firstString(item, "id", "templateId")
		if id == "" {
			continue
		}
		tmpl, err := c.getTemplate(ctx, caps, id)
		if err != nil {
			return nil, err
		}
		templates = append(templates, tmpl)
	}
	return templates, nil
}

func (c *CloudClient) getTemplate(ctx context.Context, caps *cliCapabilities, id string) (*TemplateInfo, error) {
	if len(caps.templateGetCommand) == 0 {
		return nil, fmt.Errorf("runpodctl template get is unavailable")
	}
	args := append(append([]string{}, caps.templateGetCommand...), id)
	out, err := c.runner.runOutput(ctx, caps.path, args...)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not found") {
			return nil, fmt.Errorf("%w: %s", ErrTemplateNotFound, id)
		}
		return nil, fmt.Errorf("get template %s: %w", id, err)
	}
	obj, err := decodeJSONObject(out)
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", id, err)
	}
	return templateInfoFromMap(obj), nil
}

func (c *CloudClient) createTemplate(ctx context.Context, caps *cliCapabilities, spec BootstrapTemplateSpec) (*TemplateInfo, error) {
	if len(caps.templateCreateCommand) == 0 {
		return nil, fmt.Errorf("runpodctl template create is unavailable")
	}
	args := append(append([]string{}, caps.templateCreateCommand...),
		"--name", spec.Name,
		"--image", spec.Image,
		"--docker-start-cmd", spec.StartCommand,
		"--readme", spec.Readme,
	)
	out, err := c.runner.runOutput(ctx, caps.path, args...)
	if err != nil {
		return nil, fmt.Errorf("create template: %w", err)
	}
	id, err := parseCreatedResourceID(out)
	if err != nil {
		return nil, err
	}
	return c.getTemplate(ctx, caps, id)
}

func buildCreatePodArgs(offerID string, opts cloud.CreateOpts) ([]string, error) {
	args := []string{"pod", "create",
		"--gpu-id", offerID,
		"--gpu-count", "1",
	}
	switch {
	case opts.TemplateID != "":
		args = append(args, "--template-id", opts.TemplateID)
		if opts.OnStartCmd != "" {
			return nil, fmt.Errorf("runpod templates manage startup commands; remove OnStartCmd when using template %q", opts.TemplateID)
		}
	case opts.OnStartCmd != "":
		return nil, fmt.Errorf("%w; configure runpod.bootstrap_template_id or run `weft runpod setup`", ErrPerPodStartupUnsupported)
	case opts.Image != "":
		args = append(args, "--image", opts.Image)
	default:
		return nil, fmt.Errorf("runpod create requires either TemplateID or Image")
	}
	if opts.DiskGB > 0 {
		args = append(args, "--volume-in-gb", fmt.Sprintf("%d", opts.DiskGB))
	}
	if opts.Label != "" {
		args = append(args, "--name", opts.Label)
	}
	if len(opts.EnvVars) > 0 {
		data, err := jsonMarshal(opts.EnvVars)
		if err != nil {
			return nil, fmt.Errorf("encode runpod env vars: %w", err)
		}
		args = append(args, "--env", string(data))
	}
	return args, nil
}

func parseCreatedResourceID(out []byte) (string, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return "", fmt.Errorf("create command returned empty output")
	}
	if obj, err := decodeJSONObject(out); err == nil && obj != nil {
		if id := firstString(obj, "id", "templateId", "podId"); id != "" {
			return id, nil
		}
	}
	trimmed = strings.Trim(trimmed, "\"")
	if trimmed != "" && !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return trimmed, nil
	}
	return "", fmt.Errorf("could not parse resource ID from output: %s", strings.TrimSpace(string(out)))
}

func parseCreatedPodID(out []byte) (string, error) {
	return parseCreatedResourceID(out)
}

func parseGPUTypeOutput(data []byte, constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	return parseSearchOutput(data, constraints)
}

func parseSearchOutput(data []byte, constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	rows, err := decodeJSONArray(data)
	if err != nil {
		return nil, fmt.Errorf("parse GPU offers: %w", err)
	}
	var offers []cloud.Offer
	var constraint placement.GPUConstraint
	if constraints.GPUClass != "" {
		constraint = placement.ParseGPUConstraint(constraints.GPUClass)
	}
	for _, row := range rows {
		memGB := firstInt(row, "memoryInGb", "gpuMemoryInGb", "gpuMemoryGb", "memory")
		if constraints.MinGPUMemGB > 0 && memGB < constraints.MinGPUMemGB {
			continue
		}
		if constraints.MaxGPUMemGB > 0 && memGB > constraints.MaxGPUMemGB {
			continue
		}

		maxGPUs := firstInt(row, "maxGpuCount", "gpuCount", "availableGpuCount")
		numGPUs := constraints.NumGPUs
		if numGPUs == 0 {
			numGPUs = 1
		}
		if maxGPUs > 0 && maxGPUs < numGPUs {
			continue
		}

		name := firstString(row, "displayName", "gpuType", "gpuName", "name", "id")
		if constraints.GPUClass != "" && !constraint.MatchesGPUFullName(name) && !constraint.MatchesGPU(inventory.NormalizeGPUClass(name)) {
			continue
		}

		price := firstFloat(row, "communityPrice", "securePrice", "costPerHr", "price")
		if price <= 0 {
			price = firstFloat(row, "uninterruptablePrice")
		}
		if price <= 0 {
			continue
		}

		offerID := firstString(row, "id", "gpuTypeId", "gpuId", "displayName")
		if offerID == "" {
			continue
		}
		offers = append(offers, cloud.Offer{
			ProviderID:  offerID,
			Provider:    cloud.ProviderRunpod,
			GPUName:     name,
			NumGPUs:     numGPUs,
			GPUMemGB:    float64(memGB),
			CostPerHour: price,
			Verified:    firstBool(row, "secureCloud", "verified"),
			DataCenter:  firstString(row, "dataCenterId", "dataCenter"),
			DiskSpaceGB: firstFloat(row, "diskGb", "diskSpaceGb"),
		})
	}
	return offers, nil
}

func parsePods(data []byte) ([]Pod, error) {
	rows, err := decodeJSONArray(data)
	if err != nil {
		return nil, err
	}
	pods := make([]Pod, 0, len(rows))
	for _, row := range rows {
		pods = append(pods, podFromMap(row))
	}
	return pods, nil
}

func parsePod(data []byte) (*Pod, error) {
	obj, err := decodeJSONObject(data)
	if err != nil {
		return nil, err
	}
	pod := podFromMap(obj)
	return &pod, nil
}

func podFromMap(row map[string]any) Pod {
	return Pod{
		ID:          firstString(row, "id", "podId"),
		Name:        firstString(row, "name"),
		Status:      firstString(row, "desiredStatus", "status"),
		GPUType:     firstString(row, "gpuType", "gpuName"),
		GPUCount:    firstInt(row, "gpuCount"),
		CostPerHour: firstFloat(row, "costPerHr", "price"),
		SSHHost:     firstString(row, "sshHost", "ipAddress", "publicIp"),
		SSHPort:     firstInt(row, "sshPort", "port"),
	}
}

func templateInfoFromMap(row map[string]any) *TemplateInfo {
	if row == nil {
		return nil
	}
	return &TemplateInfo{
		ID:             firstString(row, "id", "templateId"),
		Name:           firstString(row, "name"),
		Image:          firstString(row, "imageName", "image"),
		DockerStartCmd: firstString(row, "dockerStartCmd", "dockerStartCommand", "startCommand"),
		Readme:         firstString(row, "readme", "README"),
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
