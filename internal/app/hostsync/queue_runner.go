package hostsync

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/slack"
)

var (
	findHostSpecFunc         = inventory.FindHost
	ensureAgentUpToDateFunc  = agentdeploy.EnsureAgentUpToDateWithOptions
	ensureRcloneConfigFunc   = agentdeploy.EnsureRcloneConfig
	loadConfigFunc           = config.Load
	getSlackWebhookFunc      = slack.GetWebhook
	deployNotifyScriptFunc   = slack.DeployNotifyScript
	buildRunnerEnvPrefixFunc = slack.BuildRunnerEnvPrefix
	ensureRunnerStartedFunc  = func(host, envPrefix, r2Bucket string, setupTimeout time.Duration) (bool, error) {
		runner := queuerunner.NewRunner(host)
		return runner.EnsureStarted(envPrefix, r2Bucket, setupTimeout)
	}
)

// EnsureQueueRunnerStarted ensures the queue runner is present and running.
// Subprocess output from agent builds is streamed to os.Stderr; for callers
// that render their own UI (autopilot/TUI) use EnsureQueueRunnerStartedQuiet.
func EnsureQueueRunnerStarted(host string) (bool, error) {
	return ensureQueueRunnerStarted(host, agentdeploy.EnsureAgentOptions{Output: os.Stderr})
}

// EnsureQueueRunnerStartedQuiet is like EnsureQueueRunnerStarted but suppresses
// raw subprocess output and publishes build phases via
// agentdeploy.SetBuildPhase so a TUI can render them.
func EnsureQueueRunnerStartedQuiet(host string) (bool, error) {
	return ensureQueueRunnerStarted(host, agentdeploy.EnsureAgentOptions{
		Output: io.Discard,
		OnProgress: func(phase string) {
			agentdeploy.SetBuildPhase(host, phase)
			slog.Info("agent deploy progress", "component", "hostsync", "host", host, "phase", phase)
		},
	})
}

func ensureQueueRunnerStarted(host string, agentOpts agentdeploy.EnsureAgentOptions) (bool, error) {
	spec := findHostSpecFunc(host)
	if spec != nil {
		if _, err := ensureAgentUpToDateFunc(host, *spec, agentOpts); err != nil {
			return false, fmt.Errorf("agent deploy failed: %w", err)
		}
	}

	var r2Bucket string
	if cfg, err := loadConfigFunc(); err == nil && cfg != nil && cfg.Vastai.R2.Bucket != "" {
		r2Bucket = cfg.Vastai.R2.Bucket
		if err := ensureRcloneConfigFunc(host, cfg.Vastai.R2.ToCloudR2Config()); err != nil {
			slog.Warn("failed to deploy rclone config", "host", host, "error", err)
		}
	}

	slackWebhook := getSlackWebhookFunc()
	deployNotifyScriptFunc(host, slackWebhook)
	envVars := buildRunnerEnvPrefixFunc(slackWebhook)

	var setupTimeout time.Duration
	if spec != nil {
		envVars += spec.BenchmarkEnvPrefix()
		setupTimeout = spec.SetupTimeoutDuration()
	}

	started, err := ensureRunnerStartedFunc(host, envVars, r2Bucket, setupTimeout)
	if err != nil {
		return false, fmt.Errorf("queue runner start failed: %w", err)
	}
	return started, nil
}
