# Future Ideas

Ideas for future enhancements that are not currently prioritized.

## Disk Full Recovery

- Intent: make queue runners resilient when local disk exhaustion interrupts
  state writes.
- User benefit: fewer manual repairs after long jobs fail due to low disk.
- Important design judgment: if sync confirms a job is complete, repair stale
  remote "current job" pointers as part of reconciliation.

## Idle Timeout

- Intent: optionally stop jobs that appear hung based on prolonged inactivity.
- User benefit: reduce wasted GPU/CPU time on deadlocked or waiting jobs.
- Important design judgment: define what counts as activity (stdout/stderr only
  vs. broader signals) to avoid false positives.

## Resource Enforcement via cgroups

- Intent: add kernel-level CPU/memory enforcement in addition to scheduling
  constraints.
- User benefit: prevent one runaway job from starving other workloads.

## macOS Per-Job Stats

- Intent: expose per-job CPU/memory/thread metrics for macOS hosts.
- User benefit: consistent observability across Linux and macOS in job detail
  views.

## Existing-Instance Donors (Same Data Center)

- Intent: allow warm running instances to act as cache donors for new launches.
- User benefit: faster startup and lower cost by avoiding dedicated donor
  launches.
- Important design judgment: keep this policy-driven (for example, allow only
  idle donors by default) to reduce workload interference risk.

## Notification Channels

- Intent: support notifications beyond Slack.
- User benefit: teams can route completion/failure events to their existing
  communication stack.

## Agent Coaching Output

- Intent: emit structured "next step" hints alongside human-readable output.
- User benefit: better autonomous-agent chaining without sacrificing human
  readability.

## Reconnectable Stay-Attached Mode

- Intent: make log attachment resilient to transient SSH disconnects and support
  manual reattachment.
- User benefit: operators can continue monitoring long jobs without restarting
  them.

## Deferred Operation Insights

- Intent: surface queued deferred operations clearly in CLI/TUI.
- User benefit: faster diagnosis when offline hosts reconnect or lag.

## Resource-Aware Plan Scheduling

- Intent: support plan-time resource thresholds (`when`) before dispatch.
- User benefit: fewer jobs landing on busy hosts and better throughput
  predictability.

## Job Templates

- Intent: let users save and reuse common job configurations.
- User benefit: faster, less error-prone repeated submissions.

## Job Arrays

- Intent: submit parameterized batches from one command.
- User benefit: simpler sweeps and embarrassingly parallel workloads.

## On-Prem Agent (Pull Model)

- Intent: run persistent on-prem agents that pull and execute queued work
  without requiring a continuously connected laptop.
- User benefit: cloud-like autonomy for on-prem hosts.
- Important design judgment: keep a coordinator optional as an optimization
  layer for global placement, not a hard dependency for basic execution.

## Interruptible / Spot Instance Support

- Intent: allow campaign launches to target interruptible/spot capacity.
- User benefit: materially lower compute cost for checkpoint-friendly workloads.
- Important design judgment: gate this mode on workload restartability and
  checkpoint behavior.

## Better Log Management

- Intent: improve retention and searchability for long-running job logs.
- User benefit: lower local disk pressure and faster incident debugging.

## Transfer Time Prediction Extensions

- Intent: improve transfer estimates with richer observations and decay.
- User benefit: better placement and launch-time estimates.

## Anomaly Detection for Running Jobs

- Intent: detect likely-stuck or pre-OOM jobs from runtime behavior.
- User benefit: earlier intervention before large compute/time loss.

## Historical Phase Counts as Priors for Progress Estimation

- Intent: use historical phase-count distributions to improve progress priors.
- User benefit: faster-converging and more believable progress estimates.

## Auto-Retry Orphaned Jobs

- Intent: automatically requeue jobs orphaned by instance failure when safe.
- User benefit: less manual recovery after infrastructure interruptions.
- Important design judgment: bound retries to avoid loops on genuinely broken
  workloads.

## Advanced Placement Optimization

- Intent: improve placement optimization under budget/deadline/uncertainty
  constraints.
- User benefit: better cost-time tradeoffs for real operator objectives.

## Workload Clustering

- Intent: cluster historical jobs into workload types for better prediction and
  diagnostics.
- User benefit: stronger cold-start estimates and anomaly baselines.
