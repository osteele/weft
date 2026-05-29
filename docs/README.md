# Weft Documentation

This directory is organized by audience so readers can find the right level of
detail quickly.

## Start Here

- [Workflow Guide](guides/workflow-guide.md) for common end-to-end usage
- [Dashboard](guides/dashboard.md) for `weft dashboard`, the tabbed at-a-glance view set
- [Cloud GPU Instances](guides/instances.md) for rental GPU workflows (launching, monitoring, grace periods, configuration)
- [Campaigns](guides/campaigns.md) for the batching concept that groups instances launched together
- [Placement](guides/placement.md) for automatic host selection, reserved placement tags, and score reasons
- [Autopilot](guides/autopilot.md) for inspecting and unblocking the auto-placement / auto-launch engine
- [Activity Narration](guides/narrate.md) for `weft narrate` — LLM-streamed commentary on job and instance transitions
- [Claude Code Channels](guides/claude-code-channels.md) for sending Weft job events into Claude Code sessions
- [Network Resilience](guides/network-resilience.md) for disconnected/offline behavior
- [Debugging](guides/debugging.md) for operational troubleshooting
- [Iterative Weft Improvement](guides/iterative-improvement.md) for mining job history and source snapshots into product fixes

## Reference

- [Command Reference](reference/commands.md) for the full CLI command reference
- [Job Plans](reference/job-plans.md) for YAML plan syntax and execution rules
- [Logging and Progress](reference/logging-and-progress.md) for live log and progress formats
- [Estimation and Modeling](reference/estimation.md) for runtime, resource, transfer, and cost estimation

## Architecture And Design

- [Architecture](design/architecture.md) for the main system layout
- [Coordinator Architecture](design/coordinator-architecture.md) for placement and dispatch internals
- [CLI, TUI, and Core Responsibilities](design/facade-core.md) for layering boundaries
- [Comparison to SLURM](design/comparison-to-slurm.md) for scheduler tradeoffs
- [Placement Telemetry](design/placement-telemetry.md) for decision logging and offline analysis
- [Allium Specs](../specs/) for executable behavioral specifications of placement, job moves, and status sync

## Development

- [Agent Deployment](development/agent-deployment.md) for rebuilding and redeploying the remote agent
- [Manual Testing Runbook](development/manual-testing-runbook.md) for manual validation procedures

## Planning

- [Roadmap](planning/ROADMAP.md) for current gaps and planned work
- [Future Ideas](planning/IDEAS.md) for unprioritized design ideas
