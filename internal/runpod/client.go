package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
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

var fetchGPUTypesFunc = fetchGPUTypes

// CloudClient implements cloud.Client via the runpodctl CLI.
type CloudClient struct {
	cliPath         string
	runner          *cliRunner
	runLocalCommand func(context.Context, string) ([]byte, error)
	cloudType       string

	capsMu sync.Mutex
	caps   *cliCapabilities
}

var _ cloud.Client = (*CloudClient)(nil)

// officialRunpodUbuntuTemplateID is RunPod's built-in Ubuntu template that
// reliably provisions the same base image with SSH wiring.
const officialRunpodUbuntuTemplateID = "runpod-ubuntu-2204"

// NewCloudClient creates a CloudClient that uses runpodctl from PATH.
func NewCloudClient() *CloudClient {
	return NewCloudClientWithCloudType(cloud.DefaultRunpodCloudType)
}

// NewCloudClientWithCloudType creates a CloudClient that searches and creates
// RunPod pods in the requested cloud class.
func NewCloudClientWithCloudType(cloudType string) *CloudClient {
	normalized, err := cloud.NormalizeRunpodCloudType(cloudType)
	if err != nil {
		normalized = cloud.DefaultRunpodCloudType
	}
	cliPath := "runpodctl"
	return &CloudClient{
		cliPath:   cliPath,
		runner:    newCLIRunner(cliPath),
		cloudType: normalized,
		runLocalCommand: func(ctx context.Context, command string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", command)
			return cmd.CombinedOutput()
		},
	}
}

func newCloudClientForTests(runner *cliRunner) *CloudClient {
	return &CloudClient{
		cliPath:   runner.cliPath,
		runner:    runner,
		cloudType: cloud.DefaultRunpodCloudType,
		runLocalCommand: func(ctx context.Context, command string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", command)
			return cmd.CombinedOutput()
		},
	}
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

	cloudType, err := c.resolveCloudType(constraints.RunpodCloudType)
	if err != nil {
		return nil, err
	}
	constraints.RunpodCloudType = cloudType

	// runpodctl gpu list no longer returns pricing fields, so we fetch GPU
	// types with pricing via the RunPod GraphQL API directly.
	gpuTypes, err := fetchGPUTypesFunc(ctx, cloudType)
	if err != nil {
		return nil, fmt.Errorf("search offers: %w", err)
	}
	return buildOffersFromGraphQL(gpuTypes, constraints), nil
}

// ShowUser returns the authenticated RunPod account balance/spend summary.
func (c *CloudClient) ShowUser() (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	out, err := c.runner.runOutput(ctx, caps.path, "user")
	if err != nil {
		return nil, fmt.Errorf("read RunPod account: %w", err)
	}
	var user User
	if err := json.Unmarshal(out, &user); err != nil {
		return nil, fmt.Errorf("parse runpod user: %w", err)
	}
	return &user, nil
}

func (c *CloudClient) CreateInstance(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
	// Pod create can take longer than cliTimeout when runpod's API is slow;
	// give it 2 minutes rather than 30 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	caps, err := c.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if opts.RegistryAuth != nil && opts.RunpodRegistryID == "" {
		id, err := ensureRegistryAuth(ctx, opts.RegistryAuth)
		if err != nil {
			return nil, fmt.Errorf("runpod registry auth: %w", err)
		}
		opts.RunpodRegistryID = id
	}
	if opts.SSHPublicKeyFile != "" {
		if err := ensureSSHKey(ctx, c.runner, caps.path, opts.SSHPublicKeyFile); err != nil {
			return nil, fmt.Errorf("runpod ssh key: %w", err)
		}
	}
	if opts.RunpodCloudType == "" {
		opts.RunpodCloudType = c.cloudType
	}
	args, err := buildCreatePodArgs(offerID, opts)
	if err != nil {
		return nil, err
	}
	slog.Debug("runpod CreateInstance", "args", args, "offerID", offerID, "image", opts.Image)
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

func (c *CloudClient) resolveCloudType(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return cloud.NormalizeRunpodCloudType(override)
	}
	if strings.TrimSpace(c.cloudType) != "" {
		return cloud.NormalizeRunpodCloudType(c.cloudType)
	}
	return cloud.DefaultRunpodCloudType, nil
}

func ensureSSHKey(ctx context.Context, runner *cliRunner, cliPath, publicKeyFile string) error {
	keyBytes, err := os.ReadFile(publicKeyFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", publicKeyFile, err)
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" {
		return fmt.Errorf("%s is empty", publicKeyFile)
	}
	out, err := runner.runOutput(ctx, cliPath, "ssh", "list-keys")
	if err != nil {
		return fmt.Errorf("list keys: %w", err)
	}
	var payload struct {
		Keys []struct {
			Key string `json:"key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return fmt.Errorf("parse keys: %w", err)
	}
	for _, existing := range payload.Keys {
		if strings.TrimSpace(existing.Key) == key {
			return nil
		}
	}
	if _, err := runner.runOutput(ctx, cliPath, "ssh", "add-key", "--key-file", publicKeyFile); err != nil {
		return fmt.Errorf("add key: %w", err)
	}
	return nil
}

func (c *CloudClient) CreateInstanceWithProgress(offerID string, opts cloud.CreateOpts, progress cloud.ProgressFunc) (*cloud.Instance, error) {
	if progress == nil {
		progress = func(string) {}
	}
	progress("runpod: submitting pod create request")
	inst, err := c.CreateInstance(offerID, opts)
	if err != nil {
		progress("runpod: pod create request failed")
		return nil, err
	}
	progress("runpod: pod create request accepted")
	return inst, nil
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
		DataCenter:  pod.DataCenter,
		MachineID:   pod.MachineID,
	}
}

func (c *CloudClient) ListAllInstances() ([]cloud.Instance, error) {
	// RunPod's pod list API is eventually consistent: newly created pods can
	// take 1+ minute to appear. If the reconciler treats absence-from-batch
	// as "instance gone", it will prematurely declare new pods dead. Return
	// (nil, nil) to signal batch unsupported — the reconciler falls back to
	// per-instance ShowInstance calls which are consistent.
	return nil, nil
}

// ListInstancesForCleanup returns the current RunPod pod inventory for
// commands that only act on terminal database launches. Cleanup does not use
// absence from this list to classify active launches, so it can safely use the
// faster batch API that reconciliation avoids.
func (c *CloudClient) ListInstancesForCleanup() ([]cloud.Instance, error) {
	return c.listAllInstancesViaCLI()
}

func (c *CloudClient) listAllInstancesViaCLI() ([]cloud.Instance, error) {
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
		slog.Debug("runpod ShowInstance cli error", "id", instanceID, "err", err)
		// Map "pod not found" (HTTP 404) to the standard not-found error
		// so reconcile sweeps can treat destroyed pods as expected rather
		// than retrying forever.
		msg := err.Error()
		if strings.Contains(msg, "pod not found") || strings.Contains(msg, "status 404") {
			return nil, fmt.Errorf("%w: pod %s", cloud.ErrInstanceNotFound, instanceID)
		}
		return nil, fmt.Errorf("get pod %s: %w", instanceID, err)
	}

	pod, err := parsePod(out)
	if err != nil {
		return nil, fmt.Errorf("parse pod response: %w", err)
	}
	if pod.MachineID == "" || pod.DataCenter == "" {
		if detail, detailErr := fetchPodDetailFunc(ctx, instanceID); detailErr == nil {
			mergePodProviderMetadata(pod, detail)
		} else {
			slog.Debug("runpod pod detail unavailable", "id", instanceID, "err", detailErr)
		}
	}
	inst := podToInstance(*pod)
	slog.Debug("runpod ShowInstance ok", "id", instanceID, "status", inst.Status, "providerID", inst.ProviderID)
	return &inst, nil
}

func (c *CloudClient) WaitReady(instanceID string, timeout time.Duration) (*cloud.Instance, error) {
	return c.waitReady(instanceID, timeout, 5*time.Second)
}

func (c *CloudClient) waitReady(instanceID string, timeout, poll time.Duration) (*cloud.Instance, error) {
	deadline := time.Now().Add(timeout)

	seen := false
	for time.Now().Before(deadline) {
		inst, err := c.ShowInstance(instanceID)
		if err != nil {
			if seen && errors.Is(err, cloud.ErrInstanceNotFound) {
				return nil, err
			}
			time.Sleep(poll)
			continue
		}
		seen = true
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
		// "pod not found to terminate" means it's already gone — treat as success
		// so the reconciler stops retrying every pass.
		msg := err.Error()
		if strings.Contains(msg, "pod not found") || strings.Contains(msg, "status 404") {
			return nil
		}
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
	c.capsMu.Lock()
	if c.caps != nil {
		caps := c.caps
		c.capsMu.Unlock()
		return caps, nil
	}
	c.capsMu.Unlock()

	caps, err := c.runner.detectCapabilities(ctx)
	if err != nil {
		return nil, err
	}

	c.capsMu.Lock()
	if c.caps == nil {
		c.caps = caps
	}
	caps = c.caps
	c.capsMu.Unlock()
	return caps, nil
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
	startArg, err := encodeTemplateStartCommandArg(spec.StartCommand)
	if err != nil {
		return nil, fmt.Errorf("encode template start command: %w", err)
	}
	args := append(append([]string{}, caps.templateCreateCommand...),
		"--name", spec.Name,
		"--image", spec.Image,
		"--docker-start-cmd", startArg,
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
	gpuCount := opts.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	cloudType, err := cloud.NormalizeRunpodCloudType(opts.RunpodCloudType)
	if err != nil {
		return nil, err
	}
	args := []string{"pod", "create",
		"--gpu-id", offerID,
		"--gpu-count", fmt.Sprintf("%d", gpuCount),
		"--cloud-type", strings.ToUpper(cloudType),
	}
	switch {
	case opts.TemplateID != "":
		args = append(args, "--template-id", opts.TemplateID)
		if opts.OnStartCmd != "" {
			return nil, fmt.Errorf("runpod templates manage startup commands; remove OnStartCmd when using template %q", opts.TemplateID)
		}
		if opts.Image != "" {
			args = append(args, "--image", opts.Image)
		}
	case opts.OnStartCmd != "":
		return nil, fmt.Errorf("%w; configure runpod.bootstrap_template_id or run `weft runpod setup`", ErrPerPodStartupUnsupported)
	case opts.Image != "":
		if opts.Image == cloud.DefaultRunpodImage {
			// Prefer RunPod's official template for the default base image.
			// Direct --image creates have been intermittently flaky, while the
			// official template path has been consistently SSH-ready.
			args = append(args, "--template-id", officialRunpodUbuntuTemplateID)
		} else {
			args = append(args, "--image", opts.Image)
		}
	default:
		return nil, fmt.Errorf("runpod create requires either TemplateID or Image")
	}
	if opts.DiskGB > 0 {
		args = append(args, "--volume-in-gb", fmt.Sprintf("%d", opts.DiskGB))
	}
	if opts.Label != "" {
		args = append(args, "--name", opts.Label)
	}
	if opts.RunpodRegistryID != "" {
		args = append(args, "--registry-auth-id", opts.RunpodRegistryID)
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

// BootstrapFromR2 waits for RunPod SSH readiness and executes the uploaded
// bootstrap script from R2 on the pod.
func (c *CloudClient) BootstrapFromR2(ctx context.Context, podID, bucket, bootstrapKey string) error {
	return c.BootstrapFromR2WithProgress(ctx, podID, bucket, bootstrapKey, nil)
}

// ProbeDriverVersion waits for RunPod SSH readiness and queries the pod's
// NVIDIA driver version before we spend time staging workloads onto it.
func (c *CloudClient) ProbeDriverVersion(ctx context.Context, podID string) (string, error) {
	if strings.TrimSpace(podID) == "" {
		return "", fmt.Errorf("pod ID is required")
	}
	sshCmd, err := c.waitForSSHCommand(ctx, podID)
	if err != nil {
		return "", err
	}
	sshCmd = injectNonInteractiveSSHOptions(sshCmd)
	remoteCmd := `nvidia-smi --query-gpu=driver_version --format=csv,noheader`
	wrapped := fmt.Sprintf("%s %s", sshCmd, shellQuoteSingle(remoteCmd))
	out, runErr := c.runLocalCommand(ctx, wrapped)
	if runErr != nil {
		return string(out), fmt.Errorf("probe driver over SSH for pod %s: %w: %s", podID, runErr, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// BootstrapFromR2WithProgress is BootstrapFromR2 with phase callbacks for UIs
// that need to distinguish SSH readiness from the remote bootstrap command.
func (c *CloudClient) BootstrapFromR2WithProgress(ctx context.Context, podID, bucket, bootstrapKey string, progress cloud.ProgressFunc) error {
	if progress == nil {
		progress = func(string) {}
	}
	if strings.TrimSpace(podID) == "" {
		return fmt.Errorf("pod ID is required")
	}
	if strings.TrimSpace(bucket) == "" {
		return fmt.Errorf("R2 bucket is required")
	}
	if strings.TrimSpace(bootstrapKey) == "" {
		return fmt.Errorf("bootstrap key is required")
	}
	progress("waiting for SSH")
	sshCmd, err := c.waitForSSHCommand(ctx, podID)
	if err != nil {
		return err
	}
	progress("SSH ready")
	sshCmd = injectNonInteractiveSSHOptions(sshCmd)
	remoteCmd := fmt.Sprintf(`set -euo pipefail; rclone cat "r2:%s/%s" > /tmp/bootstrap.sh && bash /tmp/bootstrap.sh`, bucket, bootstrapKey)
	wrapped := fmt.Sprintf("%s %s", sshCmd, shellQuoteSingle(remoteCmd))
	progress("running bootstrap script")
	out, runErr := c.runLocalCommand(ctx, wrapped)
	if runErr != nil {
		return fmt.Errorf("run bootstrap over SSH for pod %s: %w: %s", podID, runErr, strings.TrimSpace(string(out)))
	}
	progress("bootstrap script finished")
	return nil
}

func (c *CloudClient) waitForSSHCommand(ctx context.Context, podID string) (string, error) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		if ctx.Err() != nil {
			return "", waitForSSHCommandTimeoutErr(podID, lastErr, ctx)
		}
		cmd, err := c.fetchSSHCommand(ctx, podID)
		if err == nil {
			return cmd, nil
		}
		if ctx.Err() != nil {
			return "", waitForSSHCommandTimeoutErr(podID, err, ctx)
		}
		if !isPodNotReadyError(err) {
			return "", err
		}
		lastErr = err
		// Prefer ctx cancellation over ticker wakeups when both are ready.
		select {
		case <-ctx.Done():
			return "", waitForSSHCommandTimeoutErr(podID, lastErr, ctx)
		default:
		}
		select {
		case <-ctx.Done():
			return "", waitForSSHCommandTimeoutErr(podID, lastErr, ctx)
		case <-ticker.C:
		}
	}
}

func waitForSSHCommandTimeoutErr(podID string, lastErr error, ctx context.Context) error {
	if lastErr != nil {
		return fmt.Errorf("wait for runpod ssh readiness for pod %s: %w", podID, lastErr)
	}
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("wait for runpod ssh readiness for pod %s: %w", podID, ctx.Err())
	}
	return fmt.Errorf("wait for runpod ssh readiness for pod %s: timed out", podID)
}

func (c *CloudClient) fetchSSHCommand(ctx context.Context, podID string) (string, error) {
	caps, err := c.capabilities(ctx)
	if err != nil {
		return "", err
	}
	out, err := c.runner.runOutput(ctx, caps.path, "ssh", "info", podID)
	if err != nil {
		return "", fmt.Errorf("fetch RunPod SSH command for pod %s: %w", podID, err)
	}
	return parseSSHInfoCommand(out)
}

// injectNonInteractiveSSHOptions inserts options after the leading "ssh" token
// that disable interactive host-key prompts, password fallbacks, and agent key
// enumeration. Without the noninteractive options, ssh opens /dev/tty directly
// when run from a TUI, corrupting the display and blocking on input that never
// arrives. Without IdentitiesOnly, users with many local keys can exhaust the
// server's authentication attempts before ssh reaches RunPod's generated key.
func injectNonInteractiveSSHOptions(sshCmd string) string {
	trimmed := strings.TrimSpace(sshCmd)
	if !strings.HasPrefix(trimmed, "ssh ") && trimmed != "ssh" {
		return sshCmd
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "ssh"))
	opts := []string{
		"-F /dev/null",
		"-o BatchMode=yes",
		"-o StrictHostKeyChecking=no",
		"-o UserKnownHostsFile=/dev/null",
		"-o LogLevel=ERROR",
		"-o IdentitiesOnly=yes",
		"-o IdentityAgent=none",
	}
	missing := make([]string, 0, len(opts))
	for _, opt := range opts {
		if strings.HasPrefix(opt, "-o ") {
			name := sshOptionName(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(opt, "-o ")), "-o"))
			if hasSSHOption(rest, name) {
				continue
			}
		} else if opt == "-F /dev/null" {
			if hasSSHConfigFile(rest) {
				continue
			}
		}
		missing = append(missing, opt)
	}
	if identity := cloud.SSHIdentityFile(); identity != "" {
		rest = removeSSHIdentityFiles(rest)
		rest = strings.TrimSpace("-i " + shellQuoteSingle(identity) + " " + rest)
	}
	rest = strings.TrimSpace(strings.Join(append(missing, rest), " "))
	if rest == "" {
		return "ssh"
	}
	return "ssh " + rest
}

func hasSSHConfigFile(sshArgs string) bool {
	fields := strings.Fields(sshArgs)
	for i, field := range fields {
		if field == "-F" && i+1 < len(fields) {
			return true
		}
		if strings.HasPrefix(field, "-F") && len(field) > len("-F") {
			return true
		}
	}
	return false
}

func hasSSHOption(sshArgs, optionName string) bool {
	if strings.TrimSpace(sshArgs) == "" {
		return false
	}
	optionName = strings.ToLower(strings.TrimSpace(optionName))
	fields := strings.Fields(sshArgs)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == "-o" && i+1 < len(fields) {
			if sshOptionName(fields[i+1]) == optionName {
				return true
			}
			i++
			continue
		}
		if strings.HasPrefix(field, "-o") && len(field) > len("-o") {
			if sshOptionName(strings.TrimPrefix(field, "-o")) == optionName {
				return true
			}
		}
	}
	return false
}

func hasSSHIdentityFile(sshArgs string) bool {
	fields := strings.Fields(sshArgs)
	for i, field := range fields {
		if field == "-i" && i+1 < len(fields) {
			return true
		}
		if strings.HasPrefix(field, "-i") && len(field) > len("-i") {
			return true
		}
		if field == "-o" && i+1 < len(fields) && sshOptionName(fields[i+1]) == "identityfile" {
			return true
		}
		if strings.HasPrefix(field, "-o") && sshOptionName(strings.TrimPrefix(field, "-o")) == "identityfile" {
			return true
		}
	}
	return false
}

func removeSSHIdentityFiles(sshArgs string) string {
	fields := strings.Fields(sshArgs)
	if len(fields) == 0 {
		return ""
	}
	out := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == "-i" || field == "-o" && i+1 < len(fields) && sshOptionName(fields[i+1]) == "identityfile" {
			i++
			continue
		}
		if strings.HasPrefix(field, "-i") && len(field) > len("-i") {
			continue
		}
		if strings.HasPrefix(field, "-o") && sshOptionName(strings.TrimPrefix(field, "-o")) == "identityfile" {
			continue
		}
		out = append(out, field)
	}
	return strings.Join(out, " ")
}

func sshOptionName(opt string) string {
	opt = strings.TrimSpace(opt)
	if before, _, ok := strings.Cut(opt, "="); ok {
		opt = before
	}
	return strings.ToLower(strings.TrimSpace(opt))
}

var sshCommandPattern = regexp.MustCompile(`(?m)(ssh\s+-[^\n]+|ssh\s+[^\n]+)$`)

func parseSSHInfoCommand(out []byte) (string, error) {
	obj, err := decodeJSONObject(out)
	if err == nil && obj != nil {
		if msg := strings.TrimSpace(firstString(obj, "error", "message")); msg != "" {
			return "", errors.New(msg)
		}
		cmd := strings.TrimSpace(firstString(obj, "command", "sshCommand", "ssh_command", "ssh"))
		if cmd != "" {
			return cmd, nil
		}
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("empty ssh info response")
	}
	if strings.HasPrefix(text, "ssh ") {
		return text, nil
	}
	if m := sshCommandPattern.FindString(text); strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m), nil
	}
	return "", fmt.Errorf("ssh info did not include a command")
}

func isPodNotReadyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Treat transient runpodctl network errors as "not ready" so we keep
	// polling instead of bailing. Runpod's HTTP API is occasionally flaky.
	if strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "client.timeout exceeded") || strings.Contains(msg, "tls handshake") || strings.Contains(msg, "no such host") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "eof") || strings.Contains(msg, "server closed") || strings.Contains(msg, "502 bad gateway") || strings.Contains(msg, "503 service unavailable") || strings.Contains(msg, "failed to get pods") {
		return true
	}
	return strings.Contains(msg, "not ready") || strings.Contains(msg, "pod not ready") || strings.Contains(msg, "ssh info did not include a command")
}

func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func parseGPUTypeOutput(data []byte, constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	return parseSearchOutput(data, constraints)
}

func buildOffersFromGraphQL(gpuTypes []gqlGPUType, constraints cloud.OfferConstraints) []cloud.Offer {
	cloudType, err := cloud.NormalizeRunpodCloudType(constraints.RunpodCloudType)
	if err != nil {
		return nil
	}
	var offers []cloud.Offer
	var constraint placement.GPUConstraint
	if constraints.GPUClass != "" {
		constraint = placement.ParseGPUConstraint(constraints.GPUClass)
	}
	numGPUs := constraints.NumGPUs
	if numGPUs == 0 {
		numGPUs = 1
	}
	for _, gt := range gpuTypes {
		if !runpodGPUTypeSupportsCloudType(gt.SecureCloud, gt.CommunityCloud, cloudType) {
			continue
		}
		if constraints.MinGPUMemGB > 0 && gt.MemoryInGb < constraints.MinGPUMemGB {
			continue
		}
		if gt.MaxGPUCount > 0 && gt.MaxGPUCount < numGPUs {
			continue
		}
		name := gt.DisplayName
		if name == "" {
			name = gt.ID
		}
		// RunPod's displayName is terse (e.g. "A100 SXM"), while the gpuId
		// carries the precise model (e.g. "NVIDIA A100-SXM4-80GB"). Match
		// constraints against both so job classes like "a100-sxm4-80gb" resolve.
		if constraints.GPUClass != "" {
			matched := constraint.MatchesGPUFullName(name) ||
				constraint.MatchesGPU(inventory.NormalizeGPUClass(name)) ||
				constraint.MatchesGPUFullName(gt.ID) ||
				constraint.MatchesGPU(inventory.NormalizeGPUClass(gt.ID))
			if !matched {
				continue
			}
		}
		if gt.LowestPrice == nil {
			continue
		}
		// Skip GPU types with no stock — pod create will fail.
		if gt.LowestPrice.StockStatus == nil || *gt.LowestPrice.StockStatus == "" {
			continue
		}
		var price float64
		if gt.LowestPrice.UninterruptablePrice != nil && *gt.LowestPrice.UninterruptablePrice > 0 {
			price = *gt.LowestPrice.UninterruptablePrice
		} else if gt.LowestPrice.MinimumBidPrice != nil && *gt.LowestPrice.MinimumBidPrice > 0 {
			price = *gt.LowestPrice.MinimumBidPrice
		}
		if price <= 0 {
			continue
		}
		price *= float64(numGPUs)
		offers = append(offers, cloud.Offer{
			ProviderID:  gt.ID,
			Provider:    cloud.ProviderRunpod,
			GPUName:     name,
			NumGPUs:     numGPUs,
			GPUMemGB:    float64(gt.MemoryInGb),
			CostPerHour: price,
			Verified:    gt.SecureCloud,
			StockStatus: *gt.LowestPrice.StockStatus,
		})
	}
	return offers
}

func parseSearchOutput(data []byte, constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	cloudType, err := cloud.NormalizeRunpodCloudType(constraints.RunpodCloudType)
	if err != nil {
		return nil, err
	}
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
		if !runpodGPUTypeSupportsCloudType(firstBool(row, "secureCloud"), firstBool(row, "communityCloud"), cloudType) {
			continue
		}
		memGB := firstInt(row, "memoryInGb", "gpuMemoryInGb", "gpuMemoryGb", "memory")
		if constraints.MinGPUMemGB > 0 && memGB < constraints.MinGPUMemGB {
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

		price := runpodSearchRowPrice(row, cloudType)
		if price <= 0 {
			price = firstFloat(row, "uninterruptablePrice")
		}
		if price <= 0 {
			continue
		}
		price *= float64(numGPUs)

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
			StockStatus: firstString(row, "stockStatus"),
		})
	}
	return offers, nil
}

func runpodGPUTypeSupportsCloudType(secure, community bool, cloudType string) bool {
	if !secure && !community {
		return true
	}
	switch cloudType {
	case cloud.RunpodCloudTypeSecure:
		return secure
	default:
		return community
	}
}

func runpodSearchRowPrice(row map[string]any, cloudType string) float64 {
	if cloudType == cloud.RunpodCloudTypeSecure {
		return firstFloat(row, "securePrice", "costPerHr", "price", "communityPrice")
	}
	return firstFloat(row, "communityPrice", "costPerHr", "price", "securePrice")
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
	machineID, dataCenter := podMachineMetadata(row)
	return Pod{
		ID:          firstString(row, "id", "podId"),
		Name:        firstString(row, "name"),
		Status:      firstString(row, "status", "desiredStatus"),
		GPUType:     firstString(row, "gpuType", "gpuName"),
		GPUCount:    firstInt(row, "gpuCount"),
		CostPerHour: firstFloat(row, "costPerHr", "price"),
		SSHHost:     firstString(row, "sshHost", "ipAddress", "publicIp"),
		SSHPort:     firstInt(row, "sshPort", "port"),
		DataCenter:  dataCenter,
		MachineID:   machineID,
	}
}

func mergePodProviderMetadata(pod *Pod, detail *Pod) {
	if pod == nil || detail == nil {
		return
	}
	if pod.MachineID == "" {
		pod.MachineID = detail.MachineID
	}
	if pod.DataCenter == "" {
		pod.DataCenter = detail.DataCenter
	}
}

func podMachineMetadata(row map[string]any) (machineID, dataCenter string) {
	machineID = firstString(row, "machineId", "machineID", "machine_id")
	dataCenter = firstString(row, "dataCenterId", "dataCenter", "data_center_id", "data_center", "region")
	if machine, ok := nestedStringMap(row, "machine"); ok {
		if machineID == "" {
			machineID = firstString(machine, "id", "machineId", "machineID", "machine_id")
		}
		if dataCenter == "" {
			dataCenter = firstString(machine, "dataCenterId", "dataCenter", "data_center_id", "data_center", "region")
		}
		if dataCenter == "" {
			if dc, ok := nestedStringMap(machine, "dataCenter"); ok {
				dataCenter = firstString(dc, "id", "name", "region")
			}
		}
	}
	if dataCenter == "" {
		if dc, ok := nestedStringMap(row, "dataCenter"); ok {
			dataCenter = firstString(dc, "id", "name", "region")
		}
	}
	return machineID, dataCenter
}

func nestedStringMap(row map[string]any, key string) (map[string]any, bool) {
	value, ok := row[key]
	if !ok || value == nil {
		return nil, false
	}
	nested, ok := value.(map[string]any)
	return nested, ok
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

func encodeTemplateStartCommandArg(startCommand string) (string, error) {
	startCommand = strings.TrimSpace(startCommand)
	if startCommand == "" {
		return "", fmt.Errorf("start command is empty")
	}
	if strings.HasPrefix(startCommand, "bash,-lc,") {
		return startCommand, nil
	}
	if !strings.HasPrefix(startCommand, "bash -lc ") {
		return "", fmt.Errorf("expected exec-form start command prefixed with %q", "bash -lc ")
	}
	script := strings.TrimPrefix(startCommand, "bash -lc ")
	if strings.TrimSpace(script) == "" {
		return "", fmt.Errorf("missing shell payload after %q", "bash -lc ")
	}
	if strings.Contains(script, ",") {
		return "", fmt.Errorf("command contains comma; runpodctl expects comma-separated argv for --docker-start-cmd")
	}
	return "bash,-lc," + script, nil
}
