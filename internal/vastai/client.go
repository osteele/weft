package vastai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/util"
)

// cliTimeout is the default maximum time to wait for a vastai CLI command to
// complete. Commands known to take longer when vast.ai's API is slow opt in
// to a longer per-call timeout via runWithTimeout — see createInstanceTimeout
// and attachSSHTimeout below.
const cliTimeout = 30 * time.Second

// createInstanceTimeout bounds `vastai create instance`. Vast.ai's create API
// occasionally takes well over 30s under load; a 30s ceiling turned slow
// successes into hard `provider_timeout` failures. Matches the analogous
// runpod CreateInstance timeout in internal/runpod/client.go.
const createInstanceTimeout = 2 * time.Minute

// attachSSHTimeout bounds `vastai attach ssh`. Same rationale as
// createInstanceTimeout: under load this call can exceed the default 30s.
const attachSSHTimeout = 2 * time.Minute

const availabilityGracePeriod = 2 * time.Minute
const userAPITimeout = 10 * time.Second

var vastaiUserEndpoint = "https://console.vast.ai/api/v0/users/current/"

var availabilityState struct {
	mu          sync.Mutex
	lastSuccess time.Time
}

// VastaiClient is the interface for interacting with the Vast.ai API.
type VastaiClient interface {
	Available() error
	SearchOffers(constraints OfferConstraints) ([]Offer, error)
	CreateInstance(offerID int, opts CreateOpts) (*Instance, error)
	AttachSSH(instanceID int, publicKeyFile string) error
	ShowInstance(instanceID int) (*Instance, error)
	ListAllInstances() ([]Instance, error)
	WaitReady(instanceID int, timeout time.Duration) (*Instance, error)
	DestroyInstance(instanceID int) error
	ChangeBid(instanceID int, pricePerHour float64) error
	CopyBetweenInstances(srcInstanceID int, srcPath string, dstInstanceID int, dstPath string) error
	ShowUser() (*User, error)
}

var _ VastaiClient = (*Client)(nil)

// Client wraps the vastai CLI tool.
type Client struct {
	// CLIPath is the path to the vastai binary. Defaults to "vastai".
	CLIPath string
	// CLITimeout overrides the default timeout for tests and slow provider calls.
	CLITimeout time.Duration
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
	out, err := c.run("show", "user", "--raw")
	if err != nil {
		if isVastAuthError(err) {
			return fmt.Errorf("vastai CLI authentication failed: %v", err)
		}
		if isVastTransientAvailabilityError(err) && recentlyAvailable(availabilityGracePeriod) {
			return nil
		}
		return fmt.Errorf("vastai CLI availability check failed: %v", err)
	}
	if msg := extractProviderErrorMessage(out); msg != "" {
		return fmt.Errorf("vastai CLI availability check failed: %s", msg)
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
	if c.CLIPath == "" || c.CLIPath == "vastai" {
		if apiKey := ReadAPIKey(); apiKey != "" {
			return showUserFromAPI(apiKey, vastaiUserEndpoint)
		}
	}
	out, err := c.run("show", "user", "--raw")
	if err != nil {
		return nil, fmt.Errorf("show user: %w", err)
	}
	return parseShowUserOutput(out)
}

func showUserFromAPI(apiKey, endpoint string) (*User, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("show user: missing Vast.ai API key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), userAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("show user: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("show user: request failed: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("show user: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg := extractProviderErrorMessage(out); msg != "" {
			return nil, fmt.Errorf("show user: HTTP %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("show user: HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(out)), 200))
	}
	return parseShowUserOutput(out)
}

func parseShowUserOutput(out []byte) (*User, error) {
	if msg := extractProviderErrorMessage(out); msg != "" {
		return nil, fmt.Errorf("show user: %s", msg)
	}
	if msg := extractCLIError(out); msg != "" {
		return nil, fmt.Errorf("show user: %s", msg)
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil, fmt.Errorf("show user: provider returned empty response")
	}
	var user User
	if err := json.Unmarshal(out, &user); err != nil {
		return nil, fmt.Errorf("parse user: %w (output: %s)", err, truncate(string(out), 200))
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

	// c.run prefixes errors with the operation (e.g. "search offers: …"), so
	// don't re-wrap with another "search offers:" prefix here — that produces
	// a doubled "search offers: search offers: …" headline in the TUI. The
	// internal-only branches below (empty body, parse failures) add
	// "search offers:" because c.run didn't supply context for those cases.
	out, err := c.run(args...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil, fmt.Errorf("search offers: provider returned empty response")
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
	if constraints.MinCUDAVersion != "" {
		offers = filterOffersByMinCUDA(offers, constraints.MinCUDAVersion)
	}

	// Apply GPU class post-filter for generation/family constraints
	if postFilter != nil {
		offers = postFilter(offers)
	}

	return offers, nil
}

func filterOffersByMinCUDA(offers []Offer, minCUDA string) []Offer {
	minCUDA = strings.TrimSpace(minCUDA)
	if minCUDA == "" {
		return offers
	}
	min, err := strconv.ParseFloat(minCUDA, 64)
	if err != nil {
		return nil
	}
	filtered := offers[:0]
	for _, offer := range offers {
		if offer.CUDAVersion >= min {
			filtered = append(filtered, offer)
		}
	}
	return filtered
}

// CreateInstance creates a new instance from an offer.
func (c *Client) CreateInstance(offerID int, opts CreateOpts) (*Instance, error) {
	args := buildCreateArgs(offerID, opts)

	out, err := c.runWithTimeout(createInstanceTimeout, args...)
	if err != nil {
		switch {
		case isAccountCreditError(err.Error()):
			return nil, upgradeSentinel("create-instance", cloud.ErrAccountCreditExhausted, err)
		case isUnavailableOfferError(err):
			return nil, upgradeSentinel("create-instance", cloud.ErrOfferUnavailable, err)
		case isProviderRejectedCreateError(err):
			return nil, upgradeSentinel("create-instance", cloud.ErrProviderRejected, err)
		}
		return nil, fmt.Errorf("create instance: %w", err)
	}

	// Parse response — vastai create returns {"new_contract": <id>, "success": true}
	var resp struct {
		NewContract int             `json:"new_contract"`
		Success     bool            `json:"success"`
		Error       json.RawMessage `json:"error"`
		Msg         string          `json:"msg"`
	}
	if strings.TrimSpace(string(out)) == "" {
		// Vast.ai's create endpoint returns an empty body when the account is
		// out of credit instead of a structured error. Probe ShowUser before
		// surfacing the generic provider-rejected message so the autopilot
		// stops retrying against a dead provider.
		if credit, probeErr := c.probeCreditBalance(); probeErr == nil && credit <= 0 {
			return nil, classify("create-instance", cloud.ErrAccountCreditExhausted, 0, fmt.Sprintf("provider returned empty response while account credit was exhausted ($%.2f)", credit))
		}
		return nil, classify("create-instance", cloud.ErrProviderRejected, 0, "provider returned empty response")
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		if msg := extractProviderErrorMessage(out); msg != "" {
			sentinel := cloud.ErrProviderRejected
			if isAccountCreditError(msg) {
				sentinel = cloud.ErrAccountCreditExhausted
			}
			return nil, classify("create-instance", sentinel, 0, msg)
		}
		if msg := extractCLIError(out); msg != "" {
			if isAccountCreditError(msg) {
				return nil, classify("create-instance", cloud.ErrAccountCreditExhausted, 0, msg)
			}
			return nil, fmt.Errorf("create instance: %s", msg)
		}
		return nil, fmt.Errorf("parse create response: %w (output: %s)", err, truncate(string(out), 200))
	}
	if !resp.Success {
		reason := createResponseErrorReason(resp.Error, resp.Msg)
		if reason == "" {
			reason = "provider returned success=false"
		}
		sentinel := cloud.ErrProviderRejected
		if isAccountCreditError(reason) {
			sentinel = cloud.ErrAccountCreditExhausted
		}
		if resp.NewContract != 0 {
			// Vast.ai sometimes allocates an instance even when reporting failure.
			// Destroy it to avoid an orphaned billing instance.
			slog.Warn("create returned success=false with contract, destroying orphan", "component", "vastai", "contract", resp.NewContract)
			if destroyErr := c.DestroyInstance(resp.NewContract); destroyErr != nil {
				slog.Warn("failed to destroy orphaned instance", "component", "vastai", "instance", resp.NewContract, "error", destroyErr)
			}
			return nil, fmt.Errorf("%w (contract %d)", classify("create-instance", sentinel, 0, reason), resp.NewContract)
		}
		return nil, classify("create-instance", sentinel, 0, reason)
	}

	if opts.PublicKeyFile != "" {
		if err := c.AttachSSH(resp.NewContract, opts.PublicKeyFile); err != nil {
			slog.Warn("failed to attach SSH key, destroying instance", "component", "vastai", "instance", resp.NewContract, "key", opts.PublicKeyFile, "error", err)
			if destroyErr := c.DestroyInstance(resp.NewContract); destroyErr != nil {
				slog.Warn("failed to destroy instance after SSH key attach failure", "component", "vastai", "instance", resp.NewContract, "error", destroyErr)
			}
			return nil, fmt.Errorf("attach SSH key to instance %d: %w", resp.NewContract, err)
		}
	}

	return &Instance{ID: resp.NewContract}, nil
}

// AttachSSH attaches an SSH public key to an instance.
func (c *Client) AttachSSH(instanceID int, publicKeyFile string) error {
	if strings.TrimSpace(publicKeyFile) == "" {
		return nil
	}
	if _, err := c.runWithTimeout(attachSSHTimeout, "attach", "ssh", strconv.Itoa(instanceID), publicKeyFile); err != nil {
		return fmt.Errorf("attach ssh: %w", err)
	}
	return nil
}

func buildCreateArgs(offerID int, opts CreateOpts) []string {
	args := []string{"create", "instance", strconv.Itoa(offerID)}
	// vastai's `create instance` has no --type flag; presence of --bid_price
	// is what makes the instance interruptible. Emitting --type here would
	// produce an unknown-flag error.
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
	if opts.RegistryAuth != nil {
		login := fmt.Sprintf("-u %s -p %s %s", opts.RegistryAuth.Username, opts.RegistryAuth.Password, opts.RegistryAuth.Host)
		args = append(args, "--login", login)
	}
	if opts.InstanceType == cloud.InstanceTypeInterruptible && opts.MaxBidPrice > 0 {
		args = append(args, "--bid_price", strconv.FormatFloat(opts.MaxBidPrice, 'f', 4, 64))
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

func isProviderRejectedCreateError(err error) bool {
	return false
}

// isAccountCreditError returns true when a provider message indicates the
// authenticated account is out of credit. Vast.ai surfaces this as a CLI
// "failed with error 400: Your account lacks credit" message, but the create
// endpoint sometimes responds with an empty body instead — see the empty-body
// branch in CreateInstance for the ShowUser fallback.
//
// Drift warning: internal/bidding/build.go has a SQL LIKE filter that
// excludes credit-exhaustion outcomes from the survival model using the
// SAME phrase list. If you add a phrase here, add it there too. The
// bidding query also excludes the structured TerminationReasonAccountCreditExhausted
// constant, which is the long-term replacement, but CreateInstance-time
// failures still flow through the substring path until the call-site
// audit in docs/planning/ROADMAP.md § "Structured termination reasons
// for credit exhaustion" is done.
func isAccountCreditError(msg string) bool {
	if msg == "" {
		return false
	}
	s := strings.ToLower(msg)
	switch {
	case strings.Contains(s, "account lacks credit"):
		return true
	case strings.Contains(s, "insufficient balance"):
		return true
	case strings.Contains(s, "insufficient credit"):
		return true
	case strings.Contains(s, "account credit"):
		return true
	default:
		return false
	}
}

// probeCreditBalance is overridable in tests; in production it calls ShowUser
// and returns the account credit (in dollars) or an error. Used by
// CreateInstance to attribute empty-body responses to credit exhaustion.
var probeCreditBalance = func(c *Client) (float64, error) {
	user, err := c.ShowUser()
	if err != nil || user == nil {
		if err == nil {
			err = fmt.Errorf("show user: nil response")
		}
		return 0, err
	}
	return user.Credit, nil
}

func (c *Client) probeCreditBalance() (float64, error) {
	return probeCreditBalance(c)
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
	return c.waitReady(instanceID, timeout, 5*time.Second)
}

func (c *Client) waitReady(instanceID int, timeout, poll time.Duration) (*Instance, error) {
	return waitReady(instanceID, timeout, poll, c.ShowInstance)
}

func waitReady(instanceID int, timeout, poll time.Duration, showInstance func(int) (*Instance, error)) (*Instance, error) {
	deadline := time.Now().Add(timeout)

	lastStatus := ""
	seen := false
	for time.Now().Before(deadline) {
		inst, err := showInstance(instanceID)
		if err != nil {
			if seen && errors.Is(err, cloud.ErrInstanceNotFound) {
				return nil, err
			}
			// Instance might not be visible immediately after creation
			if lastStatus == "" {
				slog.Debug("instance not visible yet, retrying", "component", "vastai", "instance", instanceID)
			}
			time.Sleep(poll)
			continue
		}
		seen = true
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
	// -y skips the interactive confirmation prompt. Without it the CLI reads
	// stdin, gets EOF, prints "Aborted." to stdout, and exits 0 — silently
	// turning every destroy into a no-op and stranding stopped instances.
	_, err := c.run("destroy", "instance", strconv.Itoa(instanceID), "-y", "--raw")
	if err != nil {
		// Already gone on the provider — destroy is idempotent. Mirrors
		// runpod/client.go DestroyInstance. Without this, the reconciler
		// at internal/campaign/instance_check.go aborts the terminal-status
		// write on every pass and the launch wedges indefinitely.
		if strings.Contains(err.Error(), fmt.Sprintf("Instance %d not found", instanceID)) {
			return nil
		}
		return fmt.Errorf("destroy instance %d: %w", instanceID, err)
	}
	return nil
}

// ChangeBid updates the bid ceiling for an existing interruptible instance.
func (c *Client) ChangeBid(instanceID int, pricePerHour float64) error {
	if pricePerHour <= 0 {
		return fmt.Errorf("change bid %d: price must be positive", instanceID)
	}
	out, err := c.run("change", "bid", strconv.Itoa(instanceID), "--price", strconv.FormatFloat(pricePerHour, 'f', 4, 64), "--raw")
	if err != nil {
		return fmt.Errorf("change bid %d: %w", instanceID, err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	var resp BidUpdate
	if err := json.Unmarshal(out, &resp); err != nil {
		if msg := extractCLIError(out); msg != "" {
			return fmt.Errorf("change bid %d: %s", instanceID, msg)
		}
		return fmt.Errorf("parse change bid response: %w (output: %s)", err, truncate(string(out), 200))
	}
	if !resp.Success {
		reason := createResponseErrorReason(resp.Error, resp.Msg)
		if reason == "" {
			reason = "provider returned success=false"
		}
		return classify("change-bid", cloud.ErrProviderRejected, 0, reason)
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

// run executes a vastai CLI command at the default timeout and returns stdout.
// Use runWithTimeout for commands whose tail-latency exceeds the default.
func (c *Client) run(args ...string) ([]byte, error) {
	return c.runWithTimeout(0, args...)
}

// runWithTimeout executes a vastai CLI command with a per-call timeout
// override. A commandTimeout of 0 means "use the default cliTimeout". The
// Client.CLITimeout field (set by tests or manual configuration) always wins
// when non-zero, so tests can squeeze any command to a tight bound.
func (c *Client) runWithTimeout(commandTimeout time.Duration, args ...string) ([]byte, error) {
	cliSemaphore <- struct{}{}
	defer func() { <-cliSemaphore }()
	timeout := commandTimeout
	if timeout <= 0 {
		timeout = cliTimeout
	}
	if c != nil && c.CLITimeout > 0 {
		timeout = c.CLITimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.CLIPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	// Build the human-facing operation prefix from positional argv only —
	// flags like "--raw" or "-y" are implementation detail noise. "search
	// offers --raw" → "search offers"; "destroy instance 123 -y --raw" →
	// "destroy instance 123". Capped at 3 positional tokens so we don't
	// inline scripts or other large trailing arguments.
	prefix := operationPrefix(args, 3)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%w: vastai %s timed out after %s", cloud.ErrProviderCommandTimeout, strings.Join(prefix, " "), timeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = fmt.Sprintf("exit %d (no stderr)", exitErr.ExitCode())
			}
			return nil, fmt.Errorf("%s: %s", strings.Join(prefix, " "), detail)
		}
		return nil, err
	}
	// vastai CLI can exit 0 with empty stdout and an error payload on stderr
	// — notably for API 400s like "bogus_field is not a valid search key".
	// Surface those as ErrProviderRejected so callers see the underlying
	// reason instead of "provider returned empty response".
	if stdout.Len() == 0 && stderr.Len() > 0 {
		if cliErr, statusCode := extractStderrErrorDetailed(stderr.Bytes()); cliErr != "" {
			// Wrap with the structured ProviderError so display surfaces can
			// coalesce by fingerprint (e.g. every job hitting the same vastai
			// 400 lands in one incident, not N look-alike buckets). The
			// embedded Sentinel keeps existing errors.Is dispatch intact for
			// retry classification.
			pe := classify(opNameFromArgs(prefix), cloud.ErrProviderRejected, statusCode, cliErr)
			return nil, fmt.Errorf("%s: %w", strings.Join(prefix, " "), pe)
		}
	}
	return stdout.Bytes(), nil
}

// operationPrefix returns the positional argv tokens (up to maxPositional)
// that identify the vastai operation for error-message prefixing. Flags
// (anything starting with "-") are dropped so error text reads as the
// semantic operation rather than the literal command line — "search offers"
// instead of "search offers --raw", "destroy instance 123" instead of
// "destroy instance 123 -y --raw".
func operationPrefix(args []string, maxPositional int) []string {
	if len(args) == 0 || maxPositional <= 0 {
		return nil
	}
	out := make([]string, 0, maxPositional)
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
		if len(out) >= maxPositional {
			break
		}
	}
	return out
}

// extractStderrErrorDetailed pulls a human-readable error message and
// upstream status_code (0 when not present) out of vastai CLI stderr. Returns
// "" when stderr looks like benign output (only warnings, progress notices,
// deprecation hints, etc.) so the caller treats the command as successful —
// vastai routinely writes informational lines to stderr while exiting 0, and
// we must not misclassify those as provider rejections. Handles two observed
// error shapes:
//   - {"error": true, "status_code": N, "msg": "..."} (API error JSON)
//   - "Warning: ..." preamble followed by an error JSON line on the next line
func extractStderrErrorDetailed(stderrBytes []byte) (string, int) {
	s := strings.TrimSpace(string(stderrBytes))
	if s == "" {
		return "", 0
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var resp struct {
			Msg        string `json:"msg"`
			StatusCode int    `json:"status_code"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err == nil {
			if msg := strings.TrimSpace(resp.Msg); msg != "" {
				return msg, resp.StatusCode
			}
		}
		// Fallback to the existing extractor for shapes we don't parse here.
		if msg := extractProviderErrorMessage([]byte(line)); msg != "" {
			return msg, 0
		}
	}
	return "", 0
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
		// EffectiveMemGB applies the +2GB safety headroom dynamically here,
		// rather than at submission time, so that requests naming a known
		// hardware ceiling (e.g. "A100 80GB") aren't pushed just above the
		// hardware's own gpu_ram (80GB), excluding every matching offer.
		// It also rolls back submit-time headroom that was baked into
		// historical jobs persisted before this resolution moved to the
		// filter boundary — those jobs replan automatically next tick.
		// Filter uses GB (response field is MB).
		parts = append(parts, fmt.Sprintf("gpu_ram>=%d", EffectiveMemGB(c.GPUClass, c.MinGPUMemGB)))
	}
	if c.MinDiskGB > 0 {
		parts = append(parts, fmt.Sprintf("disk_space>=%d", c.MinDiskGB))
	}
	if c.MinCPUCoresEffective > 0 {
		parts = append(parts, fmt.Sprintf("cpu_cores_effective>=%d", c.MinCPUCoresEffective))
	}
	if c.MinHostRAMGB > 0 {
		// cpu_ram is reported in MB.
		parts = append(parts, fmt.Sprintf("cpu_ram>=%d", c.MinHostRAMGB*1024))
	}
	if c.MinReliability > 0 {
		parts = append(parts, fmt.Sprintf("reliability>=%g", c.MinReliability))
	}
	if c.MinDriverVersion > 0 {
		// Vast.ai's driver_version field is a dotted version string ("535.86.05").
		// The CLI maps it server-side to integer driver_vers; passing a bare
		// integer (e.g. "535") fails the integer parse and the API returns 400
		// "ask_contract_offers.driver_vers gte None: query values can't be None",
		// silently blocking every offer search. Emit the major version padded as
		// "<N>.00.00" so the server-side mapping yields a valid integer.
		parts = append(parts, fmt.Sprintf("driver_version>=%d.00.00", c.MinDriverVersion))
	}
	if c.MinCUDAVersion != "" {
		parts = append(parts, fmt.Sprintf("cuda_vers>=%s", c.MinCUDAVersion))
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

	// Always require SSH and verified machines.
	// Note: gpu_frac is intentionally NOT filtered. It represents the fraction
	// of the host's GPU pool in this offer (e.g., 0.125 for 1 GPU on an 8x A100
	// host). Requiring gpu_frac=1 would restrict supply to single-GPU hosts and
	// eliminate all multi-GPU machine families (A100, H100 SXM, etc.) even when
	// only 1 GPU is requested. num_gpus already specifies the desired count.
	parts = append(parts, "direct_port_count>=1")
	parts = append(parts, "verified=true")

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

func extractProviderErrorMessage(out []byte) string {
	var resp struct {
		Error      json.RawMessage `json:"error"`
		Msg        string          `json:"msg"`
		StatusCode int             `json:"status_code"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return ""
	}
	return createResponseErrorReason(resp.Error, resp.Msg)
}

func createResponseErrorReason(errorValue json.RawMessage, msg string) string {
	if strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	if len(errorValue) == 0 || string(errorValue) == "null" {
		return ""
	}
	var errorString string
	if err := json.Unmarshal(errorValue, &errorString); err == nil {
		return strings.TrimSpace(errorString)
	}
	var hasError bool
	if err := json.Unmarshal(errorValue, &hasError); err == nil && hasError {
		return "provider returned error"
	}
	return ""
}

func truncate(s string, max int) string {
	return util.Truncate(s, max)
}
