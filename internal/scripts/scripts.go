package scripts

import _ "embed"

//go:embed queue-runner.sh
var QueueRunnerScript []byte

//go:embed migrate-queue-v2.sh
var MigrateQueueV2Script []byte

//go:embed notify-slack.sh
var NotifySlackScript []byte

//go:embed gpu-job-mapping.sh
var GPUJobMappingScript []byte
