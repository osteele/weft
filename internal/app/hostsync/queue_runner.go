package hostsync

import (
	"errors"
	"fmt"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/slack"
)

var (
	findHostSpecFunc        = inventory.FindHost
	ensureAgentUpToDateFunc = agentdeploy.EnsureAgentUpToDate
)

// EnsureQueueRunnerStarted ensures the queue runner is present and running.
func EnsureQueueRunnerStarted(host string) (bool, error) {
	if spec := findHostSpecFunc(host); spec != nil {
		if _, err := ensureAgentUpToDateFunc(host, *spec); err != nil {
			if !errors.Is(err, agentdeploy.ErrAgentNotAvailable) {
				return false, fmt.Errorf("agent deploy failed: %w", err)
			}
		}
	}

	slackWebhook := slack.GetWebhook()
	slack.DeployNotifyScript(host, slackWebhook)
	envVars := slack.BuildRunnerEnvPrefix(slackWebhook)

	runner := queuerunner.NewRunner(host)
	started, err := runner.EnsureStarted(envVars, "")
	if err != nil {
		return false, fmt.Errorf("queue runner start failed: %w", err)
	}
	return started, nil
}
