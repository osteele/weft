package vastai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

// cliTimeout is the maximum time to wait for a vastai CLI command to complete.
const cliTimeout = 30 * time.Second
const availabilityGracePeriod = 2 * time.Minute

var availabilityState struct {
	mu          sync.Mutex
	lastSuccess time.Time
}

// VastaiClient is the interface for interacting with the Vast.ai API.
type VastaiClient interface {
	Available() error
	SearchOffers(constraints OfferConstraints) ([]Offer, error)
	CreateInstance(offerID int, opts CreateOpts) (*Instance, error)
	ShowInstance(instanceID int) (*Instance, error)
	ListAllInstances() ([]Instance, error)
	WaitReady(instanceID int, timeout time.Duration) (*Instance, error)
	DestroyInstance(instanceID int) error
	CopyBetweenInstances(srcInstanceID int, srcPath string, dstInstanceID int, dstPath string) error
	ShowUser() (*User, error)
}

var _ VastaiClient = (*Client)(nil)

// Client wraps the vastai CLI tool.
type Client struct {
	// CLIPath is the path to the vastai binary. Defaults to "vastai".
	CLIPath string
}

// NewClient creates a Client that uses the vastai CLI from PATH.
func NewClient() *Client {
	return &Client{CLIPath: "vastai"}
}

// Available checks that the vastai CLI is installed and authenticated.
func (c *Client) Available() error {
	if _, err := exec.LookPath(c.CLIPath); err != nil {
		return fmt.Errorf("vastai CLI not found in PATH (install: pip install vastai)")
	}
	// Quick account check. This can fail for auth, network, or transient CLI errors.
	if _, err := c.run("show", "user", "--raw"); err != nil {
		if isVastAuthError(err) {
			return fmt.Errorf("vastai CLI authentication failed: %v", err)
		}
		if isVastTransientAvailabilityError(err) && recentlyAvailable(availabilityGracePeriod) {
			return nil
		}
		return fmt.Errorf("vastai CLI availability check failed: %v", err)
	}
	markAvailabilitySuccess(time.Now())
	return nil
}

func markAvailabilitySuccess(now time.Time) {
	availabilityState.mu.Lock()
	defer availabilityState.mu.Unlock()
	availabilityState.lastSuccess = now
}

func recentlyAvailable(window time.Duration) bool {
	if window <= 0 {
		return false
	}
	availabilityState.mu.Lock()
	defer availabilityState.mu.Unlock()
	return !availabilityState.lastSuccess.IsZero() && time.Since(availabilityState.lastSuccess) <= window
}

func isVastAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "invalid api key") ||
		strings.Contains(msg, "api key") ||
		strings.Contains(msg, "please login") ||
		strings.Contains(msg, "not authenticated")
}

func isVastTransientAvailabilityError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "temporary failure") ||
		strings.Contains(msg, "failed to resolve") ||
		strings.Contains(msg, "name resolution") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "eof")
}

// ShowUser returns the authenticated user's account information.
func (c *Client) ShowUser() (*User, error) {
	out, err := c.run("show", "user", "--raw")
	if err != nil {
		return nil, fmt.Errorf("show user: %w", err)
	}
	var user User
	if err := json.Unmarshal(out, &user); err != nil {
		return nil, fmt.Errorf("parse user: %w", err)
	}
	return &user, nil
}

// SearchOffers queries available GPU offers matching constraints.
func (c *Client) SearchOffers(constraints OfferConstraints) ([]Offer, error) {
	filter, postFilter := buildSearchFilter(constraints)
	args := []string{"search", "offers", "--raw"}
	if constraints.InstanceType == cloud.InstanceTypeInterruptible {
		args = append(args, "--type", "bid")
	}
	if filter != "" {
		args = append(args, filter)
	}

	out, err := c.run(args...)
	if err != nil {
		return nil, fmt.Errorf("search offers: %w", err)
	}

	var offers []Offer
	if err := json.Unmarshal(out, &offers); err != nil {
		if msg := extractCLIError(out); msg != "" {
			return nil, fmt.Errorf("search offers: %s", msg)
		}
		return nil, fmt.Errorf("parse offers: %w (output: %s)", err, truncate(string(out), 200))
	}

	// Compute derived fields
	for i := range offers {
		offers[i].GPUMemGB = float64(offers[i].GPUMemMB) / 1024.0
		if constraints.InstanceType == cloud.InstanceTypeInterruptible {
			offers[i].InstanceType = cloud.InstanceTypeInterruptible
		} else if offers[i].InstanceType == "" {
			offers[i].InstanceType = cloud.InstanceTypeOnDemand
		}
	}

	// Apply GPU class post-filter for generation/family constraints
	if postFilter != nil {
		offers = postFilter(offers)
	}

	return offers, nil
}

// CreateInstance creates a new instance from an offer.
func (c *Client) CreateInstance(offerID int, opts CreateOpts) (*Instance, error) {
	args := buildCreateArgs(offerID, opts)

	out, err := c.run(args...)
	if err != nil {
		if isUnavailableOfferError(err) {
			return nil, fmt.Errorf("%w: %v", cloud.ErrOfferUnavailable, err)
		}
		return nil, fmt.Errorf("create instance: %w", err)
	}

	// Parse response — vastai create returns {"new_contract": <id>, "success": true}
	var resp struct {
		NewContract int    `json:"new_contract"`
		Success     bool   `json:"success"`
		Error       string `json:"error"`
		Msg         string `json:"msg"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		if msg := extractCLIError(out); msg != "" {
			return nil, fmt.Errorf("create instance: %s", msg)
		}
		return nil, fmt.Errorf("parse create response: %w (output: %s)", err, truncate(string(out), 200))
	}
	if !resp.Success {
		reason := resp.Error
		if reason == "" {
			reason = resp.Msg
		}
		if reason == "" {
			reason = "provider returned success=false"
		}
		if resp.NewContract != 0 {
			// Vast.ai sometimes allocates an instance even when reporting failure.
			// Destroy it to avoid an orphaned billing instance.
			slog.Warn("create returned success=false with contract, destroying orphan", "component", "vastai", "contract", resp.NewContract)
			if destroyErr := c.DestroyInstance(resp.NewContract); destroyErr != nil {
				slog.Warn("failed to destroy orphaned instance", "component", "vastai", "instance", resp.NewContract, "error", destroyErr)
			}
			return nil, fmt.Errorf("create instance failed (contract %d): %w: %s", resp.NewContract, cloud.ErrProviderRejected, reason)
		}
		return nil, fmt.Errorf("create instance failed: %w: %s", cloud.ErrProviderRejected, reason)
	}

	return &Instance{ID: resp.NewContract}, nil
}

func buildCreateArgs(offerID int, opts CreateOpts) []string {
	args := []string{"create", "instance", strconv.Itoa(offerID)}
	if opts.InstanceType == cloud.InstanceTypeInterruptible {
		args = append(args, "--type", "bid")
	}
	if opts.Image != "" {
		args = append(args, "--image", opts.Image)
	}
	if opts.DiskGB > 0 {
		args = append(args, "--disk", strconv.Itoa(opts.DiskGB))
	}
	if opts.SSHEnabled {
		args = append(args, "--ssh")
	}
	if opts.OnStartCmd != "" {
		args = append(args, "--onstart-cmd", opts.OnStartCmd)
	}
	if opts.Label != "" {
		args = append(args, "--label", opts.Label)
	}
	if opts.MaxBidPrice > 0 {
		args = append(args, "--price", strconv.FormatFloat(opts.MaxBidPrice, 'f', 4, 64))
	}
	for _, capVal := range opts.CapAdd {
		capVal = strings.TrimSpace(capVal)
		if capVal == "" {
			continue
		}
		args = append(args, "--cap-add", capVal)
	}
	if len(opts.EnvVars) > 0 {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(opts.EnvVars)) {
			parts = append(parts, fmt.Sprintf("-e %s=%s", k, opts.EnvVars[k]))
		}
		args = append(args, "--env", strings.Join(parts, " "))
	}
	args = append(args, "--raw")
	return args
}

func isUnavailableOfferError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "ask") && (strings.Contains(msg, "no longer exists") || strings.Contains(msg, "does not exist") || strings.Contains(msg, "not found")):
		return true
	case strings.Contains(msg, "offer") && (strings.Contains(msg, "no longer exists") || strings.Contains(msg, "does not exist") || strings.Contains(msg, "not found") || strings.Contains(msg, "unavailable")):
		return true
	case strings.Contains(msg, "machine") && (strings.Contains(msg, "no longer available") || strings.Contains(msg, "unavailable")):
		return true
	default:
		return false
	}
}

// ShowInstance fetches the current state of an instance.
func (c *Client) ShowInstance(instanceID int) (*Instance, error) {
	out, err := c.run("show", "instances", "--raw")
	if err != nil {
		return nil, fmt.Errorf("show instances: %w", err)
	}

	var instances []Instance
	if err := json.Unmarshal(out, &instances); err != nil {
		if msg := extractCLIError(out); msg != "" {
			return nil, fmt.Errorf("show instances: %s", msg)
		}
		return nil, fmt.Errorf("parse instances: %w", err)
	}

	for _, inst := range instances {
		if inst.ID == instanceID {
			return &inst, nil
		}
	}
	return nil, fmt.Errorf("instance %d: %w", instanceID, cloud.ErrInstanceNotFound)
}

// ListAllInstances returns all instances from the user's Vast.ai account.
func (c *Client) ListAllInstances() ([]Instance, error) {
	out, err := c.run("show", "instances", "--raw")
	if err != nil {
		return nil, fmt.Errorf("show instances: %w", err)
	}

	var instances []Instance
	if err := json.Unmarshal(out, &instances); err != nil {
		if msg := extractCLIError(out); msg != "" {
			return nil, fmt.Errorf("show instances: %s", msg)
		}
		return nil, fmt.Errorf("parse instances: %w", err)
	}
	return instances, nil
}

// WaitReady polls until an instance reaches "running" status or the timeout expires.
func (c *Client) WaitReady(instanceID int, timeout time.Duration) (*Instance, error) {
	deadline := time.Now().Add(timeout)
	poll := 5 * time.Second

	lastStatus := ""
	for time.Now().Before(deadline) {
		inst, err := c.ShowInstance(instanceID)
		if err != nil {
			// Instance might not be visible immediately after creation
			if lastStatus == "" {
				slog.Debug("instance not visible yet, retrying", "component", "vastai", "instance", instanceID)
			}
			time.Sleep(poll)
			continue
		}
		if inst.Status != lastStatus {
			slog.Debug("instance status changed", "component", "vastai", "instance", instanceID, "status", inst.Status)
			lastStatus = inst.Status
		}
		if inst.Status == cloud.ProviderStatusRunning {
			return inst, nil
		}
		if inst.Status == cloud.ProviderStatusExited || inst.Status == cloud.ProviderStatusError {
			return inst, fmt.Errorf("instance %d entered state %q", instanceID, inst.Status)
		}
		time.Sleep(poll)
	}
	return nil, fmt.Errorf("instance %d not ready after %v", instanceID, timeout)
}

// DestroyInstance tears down an instance.
func (c *Client) DestroyInstance(instanceID int) error {
	_, err := c.run("destroy", "instance", strconv.Itoa(instanceID), "--raw")
	if err != nil {
		return fmt.Errorf("destroy instance %d: %w", instanceID, err)
	}
	return nil
}

// CopyBetweenInstances copies files between two Vast.ai instances using `vastai copy`.
// This uses rsync at LAN speed when instances are in the same data center.
func (c *Client) CopyBetweenInstances(srcInstanceID int, srcPath string, dstInstanceID int, dstPath string) error {
	src := fmt.Sprintf("%d:%s", srcInstanceID, srcPath)
	dst := fmt.Sprintf("%d:%s", dstInstanceID, dstPath)
	_, err := c.run("copy", src, dst)
	if err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
}

// cliSemaphore limits the number of concurrent vastai CLI processes to prevent
// process exhaustion when multiple goroutines query the API simultaneously.
var cliSemaphore = make(chan struct{}, 4)

// run executes a vastai CLI command and returns stdout.
func (c *Client) run(args ...string) ([]byte, error) {
	cliSemaphore <- struct{}{}
	defer func() { <-cliSemaphore }()
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.CLIPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Only include the first 3 args (subcommand + ID) — later args may be large scripts.
			prefix := args
			if len(prefix) > 3 {
				prefix = args[:3]
			}
			return nil, fmt.Errorf("%s: %s", strings.Join(prefix, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// buildSearchFilter constructs a vastai search filter string from constraints.
// Returns the filter string and an optional post-filter function for generation constraints.
func buildSearchFilter(c OfferConstraints) (string, func([]Offer) []Offer) {
	var parts []string
	var postFilter func([]Offer) []Offer

	if c.GPUClass != "" {
		vastaiNames, pf := resolveGPUFilter(c.GPUClass)
		if len(vastaiNames) == 1 {
			parts = append(parts, fmt.Sprintf("gpu_name=\"%s\"", vastaiNames[0]))
		} else if len(vastaiNames) > 1 {
			// Multiple exact names: use the first one in search, post-filter for all
			// (Vast.ai doesn't support OR in gpu_name filter)
			postFilter = makePostFilter(vastaiNames)
		} else {
			// No specific names — generation/family constraint uses post-filter
			postFilter = pf
		}
	}
	if c.MinGPUMemGB > 0 {
		parts = append(parts, fmt.Sprintf("gpu_ram>=%d", c.MinGPUMemGB)) // search filter uses GB (response field is MB)
	}
	if c.MaxGPUMemGB > 0 {
		parts = append(parts, fmt.Sprintf("gpu_ram<=%d", c.MaxGPUMemGB))
	}
	if c.MinDiskGB > 0 {
		parts = append(parts, fmt.Sprintf("disk_space>=%d", c.MinDiskGB))
	}
	if c.MinCPUCoresEffective > 0 {
		parts = append(parts, fmt.Sprintf("cpu_cores_effective>=%d", c.MinCPUCoresEffective))
	}
	if c.MinReliability > 0 {
		parts = append(parts, fmt.Sprintf("reliability>=%g", c.MinReliability))
	}
	numGPUs := c.NumGPUs
	if numGPUs == 0 {
		numGPUs = 1
	}
	parts = append(parts, fmt.Sprintf("num_gpus=%d", numGPUs))

	// Exclude geolocations (e.g., countries with unreliable R2 connectivity)
	excludeGeos := c.ExcludeGeos
	if len(excludeGeos) == 0 {
		excludeGeos = cloud.DefaultExcludeGeos
	}
	if len(excludeGeos) > 0 {
		quoted := make([]string, len(excludeGeos))
		for i, g := range excludeGeos {
			quoted[i] = fmt.Sprintf("'%s'", g)
		}
		parts = append(parts, fmt.Sprintf("geolocation notin [%s]", strings.Join(quoted, ",")))
	}

	// Always require SSH, verified machines, and full GPU allocation
	parts = append(parts, "direct_port_count>=1")
	parts = append(parts, "verified=true")
	parts = append(parts, "gpu_frac=1")

	return strings.Join(parts, " "), postFilter
}

// extractCLIError checks if CLI output is a plain-text error message rather than
// JSON. The vastai CLI sometimes writes errors to stdout (e.g., "failed with
// error 400: Your account lacks credit"). Returns the message or "" if the
// output doesn't look like a plain-text error.
func extractCLIError(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	if s[0] == '{' || s[0] == '[' {
		return ""
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
