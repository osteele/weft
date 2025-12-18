package scripts

import _ "embed"

//go:embed queue-runner.sh
var QueueRunnerScript []byte

//go:embed notify-slack.sh
var NotifySlackScript []byte
