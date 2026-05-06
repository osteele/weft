# Weft Documentation

This directory is organized by audience so readers can find the right level of
detail quickly.

## Start Here

- [Workflow Guide](guides/workflow-guide.md) for common end-to-end usage
- [Cloud GPU Instances](guides/instances.md) for rental GPU workflows (launching, monitoring, grace periods, configuration)
- [Campaigns](guides/campaigns.md) for the batching concept that groups instances launched together
- [Placement](guides/placement.md) for automatic host selection, reserved placement tags, and score reasons
- [Autopilot](guides/autopilot.md) for inspecting and unblocking the auto-placement / auto-launch engine
- [Activity Narration](guides/narrate.md) for `weft narrate` — LLM-streamed commentary on job and instance transitions
- [Network Resilience](guides/network-resilience.md) for disconnected/offline behavior
- [Debugging](guides/debugging.md) for operational troubleshooting

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
- [Queue Runner Concurrency](design/queue-concurrency.md) for the queue concurrency design
- [Sync Queue Architecture](design/sync-queue-design.md) for sync-worker design notes

## Development

- [Agent Deployment](development/agent-deployment.md) for rebuilding and redeploying the remote agent
- [Manual Testing Runbook](development/manual-testing-runbook.md) for manual validation procedures

## Planning

- [Campaign Roadmap](planning/ROADMAP.md) for campaign-system gaps
- [Future Ideas](planning/IDEAS.md) for unprioritized design ideas
