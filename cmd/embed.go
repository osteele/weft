package cmd

import (
	"github.com/osteele/weft/internal/scripts"
)

// Re-export from scripts package for backwards compatibility
var notifySlackScript = scripts.NotifySlackScript
var queueRunnerScript = scripts.QueueRunnerScript
