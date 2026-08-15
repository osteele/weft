# Weft Documentation

The documentation is organized by audience and depth.

## Start Here

- [Workflow Guide](guides/workflow-guide.md) for common end-to-end usage
- [Dashboard](guides/dashboard.md) for the tabbed `weft dashboard` views
- [Cloud GPU Instances](guides/instances.md) for rental GPU workflows (launching, monitoring, grace periods, configuration)
- [Campaigns](guides/campaigns.md) for the batching concept that groups instances launched together
- [Placement](guides/placement.md) for automatic host selection, reserved placement tags, and score reasons
- [Autopilot](guides/autopilot.md) for inspecting and unblocking automatic placement and launches
- [Activity Narration](guides/narrate.md) for LLM-streamed commentary from `weft narrate`
- [LLM and Claude Integrations](guides/llm-integrations.md) for which features call Anthropic/OpenRouter or `claude -p`
- [Claude Code Channels](guides/claude-code-channels.md) for sending Weft job events into Claude Code sessions
- [Agent-Oriented Workflows](guides/agent-workflows.md) for CLI features designed for coding agents and unattended automation
- [Network Resilience](guides/network-resilience.md) for disconnected/offline behavior
- [Debugging](guides/debugging.md) for operational troubleshooting
- [Iterative Weft Improvement](guides/iterative-improvement.md) for mining job history and source snapshots into product fixes

## Reference

- [Command Reference](reference/commands.md) for CLI syntax and behavior
- [Job Plans](reference/job-plans.md) for YAML plan syntax and execution rules
- [Logging and Progress](reference/logging-and-progress.md) for live log and progress formats
- [Estimation and Modeling](reference/estimation.md) for runtime, resource, transfer, and cost estimation

## Architecture and Design

- [Decision Records](decisions/) for why the architecture is the way it is
- [Architecture](design/architecture.md) for the main system layout
- [Subsystem Architecture](architecture/) for [placement](architecture/placement.md), [compatibility](architecture/compatibility.md), [disk estimation](architecture/disk-estimation.md), [sync](architecture/sync.md), [job lifecycle](architecture/job-lifecycle.md), and [instance lifecycle](architecture/instance-lifecycle.md)
- [Deprecated Coordinator Architecture](design/coordinator-architecture.md) for historical context only
- [CLI, TUI, and Core Responsibilities](design/facade-core.md) for layering boundaries
- [Comparison to SLURM](design/comparison-to-slurm.md) for scheduler tradeoffs
- [Placement Telemetry](design/placement-telemetry.md) for decision logging and offline analysis
- [Allium Specs](../specs/README.md) for executable behavioral specifications
  and validator setup

## Development

- [Agent Deployment](development/agent-deployment.md) for rebuilding and redeploying the remote agent
- [Manual Testing Runbook](development/manual-testing-runbook.md) for manual validation procedures

## Planning

- [Roadmap](planning/ROADMAP.md) for current gaps and planned work
- [General Compatibility Model](planning/compatibility-model.md) for the proposed job↔image↔host requirement redesign
- [Future Ideas](planning/IDEAS.md) for unprioritized design ideas
