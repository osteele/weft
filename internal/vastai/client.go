package vastai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

// cliTimeout is the maximum time to wait for a vastai CLI command to complete.
const cliTimeout = 30 * time.Second

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
	path, err := exec.LookPath(c.CLIPath)
	if err != nil {
		return fmt.Errorf("vastai CLI not found in PATH (install: pip install vastai)")
	}
	// Quick auth check: "vastai show user" fails if not authenticated
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "show", "user", "--raw")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("vastai CLI not authenticated: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// SearchOffers queries available GPU offers matching constraints.
func (c *Client) SearchOffers(constraints OfferConstraints) ([]Offer, error) {
	filter, postFilter := buildSearchFilter(constraints)
	args := []string{"search", "offers", "--raw"}
	if filter != "" {
		args = append(args, filter)
	}

	out, err := c.run(args...)
	if err != nil {
		return nil, fmt.Errorf("search offers: %w", err)
	}

	var offers []Offer
	if err := json.Unmarshal(out, &offers); err != nil {
		return nil, fmt.Errorf("parse offers: %w (output: %s)", err, truncate(string(out), 200))
	}

	// Compute derived fields
	for i := range offers {
		offers[i].GPUMemGB = float64(offers[i].GPUMemMB) / 1024.0
	}

	// Apply GPU class post-filter for generation/family constraints
	if postFilter != nil {
		offers = postFilter(offers)
	}

	return offers, nil
}

// CreateInstance creates a new instance from an offer.
func (c *Client) CreateInstance(offerID int, opts CreateOpts) (*Instance, error) {
	args := []string{"create", "instance", strconv.Itoa(offerID)}

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
	if len(opts.EnvVars) > 0 {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(opts.EnvVars)) {
			parts = append(parts, fmt.Sprintf("-e %s=%s", k, opts.EnvVars[k]))
		}
		args = append(args, "--env", strings.Join(parts, " "))
	}
	args = append(args, "--raw")

	out, err := c.run(args...)
	if err != nil {
		if isUnavailableOfferError(err) {
			return nil, fmt.Errorf("%w: %v", cloud.ErrOfferUnavailable, err)
		}
		return nil, fmt.Errorf("create instance: %w", err)
	}

	// Parse response — vastai create returns {"new_contract": <id>, "success": true}
	var resp struct {
		NewContract int  `json:"new_contract"`
		Success     bool `json:"success"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse create response: %w (output: %s)", err, truncate(string(out), 200))
	}
	if !resp.Success {
		return nil, fmt.Errorf("create instance failed: %s", string(out))
	}

	return &Instance{ID: resp.NewContract}, nil
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
				log.Printf("vastai: instance %d not visible yet, retrying...", instanceID)
			}
			time.Sleep(poll)
			continue
		}
		if inst.Status != lastStatus {
			log.Printf("vastai: instance %d status: %s", instanceID, inst.Status)
			lastStatus = inst.Status
		}
		if inst.Status == "running" {
			return inst, nil
		}
		if inst.Status == "exited" || inst.Status == "error" {
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

// run executes a vastai CLI command and returns stdout.
func (c *Client) run(args ...string) ([]byte, error) {
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

	// Always require SSH and verified machines
	parts = append(parts, "direct_port_count>=1")
	parts = append(parts, "verified=true")

	return strings.Join(parts, " "), postFilter
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
