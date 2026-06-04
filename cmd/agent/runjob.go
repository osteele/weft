package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/runner"
)

// runJob handles the "run-job" subcommand.
// Usage: weft-agent run-job --job-id=123 --log-dir=/tmp/weft-logs [--working-dir=/path/to/project]
// Reads JSON opsqueue.CommandJob from stdin.
func runJob(args []string) {
	var jobID int64
	var logDir string
	var workingDir string
	var maxTime time.Duration

	for _, arg := range args {
		switch {
		case hasPrefix(arg, "--job-id="):
			val := arg[len("--job-id="):]
			var err error
			jobID, err = strconv.ParseInt(val, 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid --job-id: %s\n", val)
				os.Exit(1)
			}
		case hasPrefix(arg, "--log-dir="):
			logDir = arg[len("--log-dir="):]
		case hasPrefix(arg, "--working-dir="):
			workingDir = arg[len("--working-dir="):]
		case hasPrefix(arg, "--max-time="):
			val := arg[len("--max-time="):]
			var err error
			maxTime, err = time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid --max-time: %s\n", val)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", arg)
			fmt.Fprintf(os.Stderr, "usage: weft-agent run-job --job-id=ID --log-dir=DIR [--working-dir=DIR] [--max-time=DURATION]\n")
			os.Exit(1)
		}
	}

	if jobID == 0 {
		fmt.Fprintln(os.Stderr, "missing required --job-id flag")
		os.Exit(1)
	}
	if logDir == "" {
		fmt.Fprintln(os.Stderr, "missing required --log-dir flag")
		os.Exit(1)
	}

	// Read job spec from stdin
	var job opsqueue.CommandJob
	if err := json.NewDecoder(os.Stdin).Decode(&job); err != nil {
		fmt.Fprintf(os.Stderr, "failed to read job from stdin: %v\n", err)
		os.Exit(1)
	}

	cfg := runner.SingleJobConfig{
		JobID:      jobID,
		Job:        job,
		LogDir:     logDir,
		WorkingDir: workingDir,
		MaxTime:    maxTime,
	}

	ei, err := runner.RunSingleJob(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-job failed: %v\n", err)
		os.Exit(1)
	}

	os.Exit(ei.ExitCode)
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
