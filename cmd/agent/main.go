package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/agentenv"
	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/runner"
)

// version is set via -ldflags "-X main.version=<agent-version>".
// The value is a deterministic local source hash in normal builds.
var version = "dev"

// agentUsageText names every valid subcommand. An unrecognized argument
// must exit with this text: a subcommand missing from it would turn a
// typo into a hard failure on a host runner or cloud instance.
const agentUsageText = `usage: weft-agent <subcommand> [args]

subcommands:
  version | --version   print the agent version and exit
  run-queue             run the queue runner (tmux-managed)
  run-job               run a single queued job
  run-instance          run a cloud instance worker (run-campaign is an alias)
  grace-wait            poll R2 for control messages during the grace period
  r2                    host-side R2 content helpers (content-info, put-content)
  heartbeat-sidecar     upload instance heartbeats to R2
  batch-status          print compact status for job ids
`

func main() {
	os.Exit(runAgentArgs(os.Args))
}

// runAgentArgs dispatches an os.Args-style argument vector. Bare invocation
// has no caller in this repository and no behavior beyond idling until a
// signal; over ssh with a timeout it left an orphaned agent on the remote
// host. There is no fall-through into starting an agent from here.
func runAgentArgs(args []string) int {
	if len(args) > 1 {
		return runAgentSubcommand(args[1], args[2:])
	}
	fmt.Fprint(os.Stderr, agentUsageText)
	return 2
}

// runAgentSubcommand dispatches one subcommand and returns the process exit
// code. There is no fall-through: anything unrecognized is a usage error,
// never an agent start.
func runAgentSubcommand(name string, rest []string) int {
	switch name {
	case "--version", "version":
		fmt.Printf("weft-agent %s\n", version)
		return 0

	case "run-queue":
		runQueue(rest)
		return 0

	case "run-job":
		runJob(rest)
		return 0

	// run-campaign is kept as a compatibility alias for manifests and
	// cached agents that still use it.
	case "run-instance", "run-campaign":
		runInstance(rest)
		return 0

	case "grace-wait":
		graceWait(rest)
		return 0

	case "r2":
		runR2Command(rest)
		return 0

	case "heartbeat-sidecar":
		runHeartbeatSidecar(rest)
		return 0

	case "batch-status":
		jobIDs, err := parseBatchStatusArgs(rest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "batch-status: %v\n", err)
			return 2
		}
		batchStatus(jobIDs)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "weft-agent: unknown subcommand %q\n\n%s", name, agentUsageText)
		return 2
	}
}

func runQueue(args []string) {
	agentenv.EnsureToolPath()
	parsed, err := parseRunQueueArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-queue: %v\n", err)
		os.Exit(2)
	}
	cfg := runner.DefaultConfig()
	cfg.SetupTimeout = parsed.SetupTimeout
	r := runner.New(cfg)
	r.AgentVersion = version
	if parsed.R2Bucket != "" {
		setupInventoryR2(r, parsed.R2Bucket)
		// Wire the Layer D R2-isolated source fallback: when the dispatcher
		// queues a job with SourceR2Key set, the runner asks us to fetch
		// the content-addressed tarball and extract it into a per-job dir.
		bucket := parsed.R2Bucket
		r.EnsureSourceFromR2 = func(_ int64, r2Key, perJobDir string) error {
			return fetchSourceTarballToDir(bucket, r2Key, perJobDir)
		}
		r.EnsureSourceManifestFromR2 = func(_ int64, manifest opsqueue.SourceManifest, perJobRoot string) (string, error) {
			return fetchSourceManifestToDir(bucket, manifest, perJobRoot)
		}
		r.EnsurePayloadsFromR2 = func(jobID int64, payloads []opsqueue.Payload) (string, error) {
			return artifacts.StagePayloads(jobID, payloads, func(key, destination string) error {
				return copyCloudNeedFromR2(bucket, key, destination)
			})
		}
		r.EnsureArtifactNeedsFromR2 = func(jobID int64, workDir string, needs []opsqueue.ArtifactNeed) error {
			return stageArtifactNeeds(bucket, jobID, workDir, needs)
		}
	}
	if r.EnsurePayloadsFromR2 != nil {
		r.Capabilities = append(r.Capabilities, opsqueue.CapabilityJobPayloadV1)
	}
	if r.EnsureArtifactNeedsFromR2 != nil {
		r.Capabilities = append(r.Capabilities, opsqueue.CapabilityArtifactNeedV1)
		r.ArtifactNeedVersion = opsqueue.ArtifactNeedVersionProducerArtifacts
	}
	if parsed.R2QueueHost != "" {
		if parsed.R2Bucket == "" {
			fmt.Fprintln(os.Stderr, "run-queue: --r2-queue-host requires --r2-bucket")
			os.Exit(2)
		}
		stop := startInventoryQueueR2(parsed.R2Bucket, parsed.R2QueueHost, cfg.QueueDir, version)
		defer stop()
	}
	if err := r.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "runner error: %v\n", err)
		os.Exit(1)
	}
}

type runQueueArgs struct {
	R2Bucket     string
	R2QueueHost  string
	SetupTimeout time.Duration
}

func parseRunQueueArgs(args []string) (runQueueArgs, error) {
	var result runQueueArgs
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--r2-bucket="):
			result.R2Bucket = arg[len("--r2-bucket="):]
		case strings.HasPrefix(arg, "--r2-queue-host="):
			result.R2QueueHost = arg[len("--r2-queue-host="):]
		case strings.HasPrefix(arg, "--setup-timeout="):
			d, err := time.ParseDuration(arg[len("--setup-timeout="):])
			if err != nil {
				return result, fmt.Errorf("invalid --setup-timeout: %w", err)
			}
			result.SetupTimeout = d
		case arg == opsqueue.AgentLegacyQueueArg:
			// Accept the legacy positional default queue name for compatibility.
		default:
			return result, fmt.Errorf("unsupported queue %q; only %q is supported", arg, opsqueue.AgentLegacyQueueArg)
		}
	}
	return result, nil
}
