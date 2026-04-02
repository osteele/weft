package hostsync

import (
	"fmt"
	"log/slog"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/slack"
)

var (
	findHostSpecFunc         = inventory.FindHost
	ensureAgentUpToDateFunc  = agentdeploy.EnsureAgentUpToDate
	ensureRcloneConfigFunc   = agentdeploy.EnsureRcloneConfig
	loadConfigFunc           = config.Load
	getSlackWebhookFunc      = slack.GetWebhook
	deployNotifyScriptFunc   = slack.DeployNotifyScript
	buildRunnerEnvPrefixFunc = slack.BuildRunnerEnvPrefix
	ensureRunnerStartedFunc  = func(host, envPrefix, r2Bucket string) (bool, error) {
		runner := queuerunner.NewRunner(host)
		return runner.EnsureStarted(envPrefix, r2Bucket)
	}
)

// EnsureQueueRunnerStarted ensures the queue runner is present and running.
func EnsureQueueRunnerStarted(host string) (bool, error) {
	if spec := findHostSpecFunc(host); spec != nil {
		if _, err := ensureAgentUpToDateFunc(host, *spec); err != nil {
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

	started, err := ensureRunnerStartedFunc(host, envVars, r2Bucket)
	if err != nil {
		return false, fmt.Errorf("queue runner start failed: %w", err)
	}
	return started, nil
}
