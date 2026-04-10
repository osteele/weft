package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/slack"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var hostSetupNoRunner bool
var hostSetupSkipPrereqs bool

var hostSetupCmd = &cobra.Command{
	Use:   "setup <hostname>",
	Short: "Set up a host for use with weft",
	Long: `Perform full host setup: install prerequisites, discover hardware,
deploy the agent binary, configure rclone, deploy Slack notifications,
and start the queue runner.

Each step is best-effort — failures are warned about but don't stop
subsequent steps (except SSH connectivity which fails fast).

Example:
  weft host setup cool30
  weft host setup cool30 --no-runner
  weft host setup cool30 --skip-prerequisites`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runHostSetup,
}

func init() {
	hostSetupCmd.Flags().BoolVar(&hostSetupNoRunner, "no-runner", false, "Skip starting the queue runner")
	hostSetupCmd.Flags().BoolVar(&hostSetupSkipPrereqs, "skip-prerequisites", false, "Skip system package installation")
}

func runHostSetup(cmd *cobra.Command, args []string) error {
	host := args[0]

	// Count total steps (adjust for skipped steps)
	totalSteps := 6
	if hostSetupSkipPrereqs {
		totalSteps--
	}
	if hostSetupNoRunner {
		totalSteps--
	}

	step := 0
	nextStep := func() int {
		step++
		return step
	}

	fmt.Fprintf(os.Stderr, "Setting up %s...\n\n", host)

	// Step 1: Install prerequisites (unless --skip-prerequisites)
	if !hostSetupSkipPrereqs {
		fmt.Fprintf(os.Stderr, "  [%d/%d] Installing prerequisites...", nextStep(), totalSteps)
		installed, err := installPrerequisites(host)
		if err != nil {
			fmt.Fprintf(os.Stderr, " warning: %v\n", err)
		} else if len(installed) > 0 {
			fmt.Fprintf(os.Stderr, " installed %s\n", strings.Join(installed, " "))
		} else {
			fmt.Fprintf(os.Stderr, " all present\n")
		}
	}

	// Step 2: Discover host hardware → write YAML
	fmt.Fprintf(os.Stderr, "  [%d/%d] Discovering hardware...", nextStep(), totalSteps)
	spec, yamlPath, err := discoverHost(host)
	if err != nil {
		fmt.Fprintf(os.Stderr, " FAILED\n")
		return fmt.Errorf("discover host: %w", err)
	}
	fmt.Fprintf(os.Stderr, " %s\n", formatDiscoverySummary(spec))
	fmt.Fprintf(os.Stderr, "        Wrote %s\n", yamlPath)

	// Step 4: Deploy agent binary
	fmt.Fprintf(os.Stderr, "  [%d/%d] Deploying agent...", nextStep(), totalSteps)
	deployed, agentErr := agentdeploy.EnsureAgentUpToDate(host, spec)
	agentReady := agentErr == nil
	if agentErr != nil {
		if errors.Is(agentErr, agentdeploy.ErrAgentNotAvailable) {
			fmt.Fprintf(os.Stderr, " skipped (no agent builder succeeded)\n")
		} else {
			fmt.Fprintf(os.Stderr, " warning: %v\n", agentErr)
		}
	} else if deployed {
		ver, _ := agentdeploy.LocalAgentVersion()
		if ver != "" {
			fmt.Fprintf(os.Stderr, " deployed (version %s)\n", ver)
		} else {
			fmt.Fprintf(os.Stderr, " deployed\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, " up to date\n")
	}

	// Step 5: Deploy rclone config (if R2 configured)
	fmt.Fprintf(os.Stderr, "  [%d/%d] Deploying rclone config...", nextStep(), totalSteps)
	var r2Bucket string
	if cfg, cfgErr := config.Load(); cfgErr == nil && cfg.Vastai.R2.Bucket != "" {
		r2Bucket = cfg.Vastai.R2.Bucket
		r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
		if rcloneErr := agentdeploy.EnsureRcloneConfig(host, r2Cfg); rcloneErr != nil {
			fmt.Fprintf(os.Stderr, " warning: %v\n", rcloneErr)
		} else {
			fmt.Fprintf(os.Stderr, " ok\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, " skipped (no R2 config)\n")
	}

	// Step 6: Deploy Slack notify script
	fmt.Fprintf(os.Stderr, "  [%d/%d] Deploying Slack notifications...", nextStep(), totalSteps)
	slackWebhook := slack.GetWebhook()
	if slackWebhook != "" {
		slack.DeployNotifyScript(host, slackWebhook)
		fmt.Fprintf(os.Stderr, " ok\n")
	} else {
		fmt.Fprintf(os.Stderr, " skipped (no webhook)\n")
	}

	// Step 7: Start queue runner (unless --no-runner)
	if !hostSetupNoRunner {
		fmt.Fprintf(os.Stderr, "  [%d/%d] Starting queue runner...", nextStep(), totalSteps)
		if !agentReady {
			fmt.Fprintf(os.Stderr, " skipped (agent unavailable)\n")
		} else {
			envVars := slack.BuildRunnerEnvPrefix(slackWebhook)
			runner := queuerunner.NewRunner(host)
			started, runnerErr := runner.EnsureStarted(envVars, r2Bucket, spec.SetupTimeoutDuration())
			if runnerErr != nil {
				fmt.Fprintf(os.Stderr, " warning: %v\n", runnerErr)
			} else if started {
				fmt.Fprintf(os.Stderr, " started\n")
			} else {
				fmt.Fprintf(os.Stderr, " already running\n")
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\nSetup complete. Host %s is ready.\n", host)
	return nil
}

// installPrerequisites checks for required CLI tools and installs missing ones.
// Returns the list of packages that were installed.
func installPrerequisites(host string) ([]string, error) {
	// Probe for missing commands and detect package manager in a single SSH call
	probeCmd := `echo "---MISSING---"
for cmd in tmux jq rsync rclone curl; do command -v "$cmd" >/dev/null 2>&1 || echo "$cmd"; done
echo "---PKGMGR---"
command -v apt-get >/dev/null 2>&1 && echo apt-get || (command -v brew >/dev/null 2>&1 && echo brew || echo none)`
	stdout, _, err := ssh.Run(host, probeCmd)
	if err != nil {
		return nil, fmt.Errorf("probe commands: %w", err)
	}

	// Parse the combined output
	missingSection, pkgMgrSection := "", ""
	if idx := strings.Index(stdout, "---PKGMGR---"); idx >= 0 {
		missingSection = stdout[strings.Index(stdout, "---MISSING---")+len("---MISSING---") : idx]
		pkgMgrSection = stdout[idx+len("---PKGMGR---"):]
	}

	missing := parseLines(missingSection)
	if len(missing) == 0 {
		return nil, nil
	}

	pkgMgr := strings.TrimSpace(pkgMgrSection)
	if pkgMgr == "none" || pkgMgr == "" {
		return nil, fmt.Errorf("cannot install %s: no package manager found", strings.Join(missing, ", "))
	}

	var installCmd string
	switch pkgMgr {
	case "apt-get":
		installCmd = fmt.Sprintf("sudo apt-get install -y %s", strings.Join(missing, " "))
	case "brew":
		installCmd = fmt.Sprintf("brew install %s", strings.Join(missing, " "))
	default:
		return nil, fmt.Errorf("unsupported package manager %q for installing %s", pkgMgr, strings.Join(missing, ", "))
	}

	if _, stderr, err := ssh.Run(host, installCmd); err != nil {
		return nil, fmt.Errorf("install failed: %w\n%s", err, stderr)
	}

	return missing, nil
}

// discoverHost probes a host via SSH and writes its inventory YAML file.
// Returns the discovered HostSpec and the path to the written YAML file.
func discoverHost(host string) (inventory.HostSpec, string, error) {
	stdout, stderr, err := ssh.Run(host, hostinfo.HostInfoCommand)
	if err != nil {
		return inventory.HostSpec{}, "", fmt.Errorf("SSH to %s: %w\n%s", host, err, stderr)
	}

	info := hostinfo.ParseHostInfo(stdout)

	// Detect HF cache directory
	hfCacheDir := ""
	if hfOut, _, hfErr := ssh.Run(host, inventory.DetectHFCacheDirCommand()); hfErr == nil {
		hfCacheDir = hfOut
	} else {
		fmt.Fprintf(os.Stderr, "Warning: could not detect HF cache dir on %s: %v\n", host, hfErr)
	}

	spec := inventory.HostSpecFromHostInfo(host, info, hfCacheDir)

	// Ensure hosts directory exists
	hostsDir := inventory.HostsDir()
	if err := os.MkdirAll(hostsDir, 0755); err != nil {
		return inventory.HostSpec{}, "", fmt.Errorf("create hosts dir: %w", err)
	}

	// Marshal to YAML
	data, err := yaml.Marshal(spec)
	if err != nil {
		return inventory.HostSpec{}, "", fmt.Errorf("marshal YAML: %w", err)
	}

	header := "# Auto-generated by `weft host discover`. Adjust cpu_factor/gpu_factor manually.\n"
	outPath := filepath.Join(hostsDir, host+".yaml")
	if err := os.WriteFile(outPath, []byte(header+string(data)), 0644); err != nil {
		return inventory.HostSpec{}, "", fmt.Errorf("write %s: %w", outPath, err)
	}

	return spec, outPath, nil
}

func formatDiscoverySummary(spec inventory.HostSpec) string {
	parts := []string{}
	if len(spec.GPUs) > 0 {
		parts = append(parts, formatGPUSummary(spec.GPUs))
	}
	if spec.Memory != "" {
		parts = append(parts, spec.Memory+" RAM")
	}
	if spec.CPUCores > 0 {
		parts = append(parts, fmt.Sprintf("%d cores", spec.CPUCores))
	}
	if len(parts) == 0 {
		return "ok"
	}
	return strings.Join(parts, ", ")
}

func parseLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
