package cmd

import (
	_ "embed"
)

//go:embed notify-slack.sh
var notifySlackScript []byte

//go:embed queue-runner.sh
var queueRunnerScript []byte
