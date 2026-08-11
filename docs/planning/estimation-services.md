# Estimation Services Plan

Status: **Plan** — describes intended design, not current implementation.

## Context

Weft has a rich set of estimation capabilities that are currently consumed only
internally for placement scoring, cost modeling, and TUI display. External tools
— SkyPilot, SLURM site schedulers, custom workflow managers — face similar
estimation problems but lack these models.

This plan describes how weft could expose its estimators as a set of CLI
subcommands that external tools can call, analogous to how the external
`job-estimator` Python package provides duration/memory prediction services to
weft.

## Motivation

Weft's estimators go beyond what `job-estimator` provides. `job-estimator`
predicts duration, peak RSS, and peak GPU memory for a single command.
Weft layers on top of that:

| Capability | `job-estimator` | Weft |
|---|---|---|
| Job duration (with quantiles) | Yes | Yes (wraps job-estimator) |
| Peak RSS memory | Yes | Yes |
| Peak GPU memory | Yes | Yes |
| GPU performance scaling | — | Local→cloud runtime via GPU generation roofline |
| Cloud overhead phases | — | Hierarchical Bayesian model for SSH setup, job setup, upload |
| Instance startup time | — | Empirical distribution (45s mean, 30s–3min bounds) |
| Provisioning time | — | Workdir sync + model download + UV cold-install |
| Transfer bandwidth | — | Learned EMA per (source, dest) pair |
| Completion time (per host) | — | Monte Carlo simulation of queue wait + runtime |
| Cloud survival probability | — | Beta-Binomial model per GPU family / price bucket |
| Risk-adjusted cloud cost | — | Expected cost including retry overhead |
| Job progress / phase count | — | Bayesian estimation of total phases from partial observation |
| UV sync byte estimation | — | Cold Python dependency install size from lockfile hash |

An external scheduler that can call `weft estimate duration ...` or
`weft estimate cost ...` gains access to these models without reimplementing
them.

## Proposed CLI Interface

All commands output JSON by default (machine-readable). Add `--human` for
formatted output.

### Duration and resource estimation

```bash
# Predict duration, RSS, GPU memory for a command on a given GPU
weft estimate duration \
  --command "python train.py --model meta-llama/Meta-Llama-3-8B" \
  --gpu "RTX 3090" \
  --gpu-mem 24GB

# Output:
{
  "duration_s": {"mean": 3420, "p10": 2100, "p50": 3200, "p90": 5400},
  "peak_rss_kb": {"mean": 8200000, "p10": 6100000, "p90": 11000000},
  "peak_gpu_mem_mib": {"mean": 18200, "p10": 15000, "p90": 22000}
}
```

### GPU performance scaling

```bash
# Estimate runtime on a target GPU given a known runtime on a source GPU
weft estimate scale-runtime \
  --source-gpu "RTX 3090" \
  --target-gpu "A100 80GB" \
  --duration 3600

# Output:
{
  "scaled_duration_s": {"mean": 1850, "lower": 1200, "upper": 2700},
  "performance_ratio": 1.95
}
```

### Transfer time

```bash
# Estimate transfer time for a model or dataset
weft estimate transfer \
  --source hf:meta-llama/Meta-Llama-3-8B \
  --dest cool30

# Output:
{
  "size_bytes": 16064000000,
  "bandwidth_mbps": {"learned": 850, "source": "ema"},
  "transfer_s": {"mean": 151, "lower": 75, "upper": 302}
}
```

### Cloud cost

```bash
# Estimate total cost for a job on a cloud GPU class
weft estimate cloud-cost \
  --command "python train.py --model meta-llama/Meta-Llama-3-8B" \
  --gpu "A100 80GB" \
  --inputs hf:meta-llama/Meta-Llama-3-8B \
  --rate-per-hour 1.20

# Output:
{
  "phases": {
    "startup_s": {"mean": 45, "lower": 30, "upper": 180},
    "provision_s": {"mean": 320, "lower": 180, "upper": 600},
    "runtime_s": {"mean": 1850, "lower": 1200, "upper": 2700},
    "upload_s": {"mean": 15, "lower": 8, "upper": 35}
  },
  "total_s": {"mean": 2230, "lower": 1418, "upper": 3515},
  "cost_usd": {"mean": 0.74, "lower": 0.47, "upper": 1.17},
  "survival_probability": 0.92,
  "risk_adjusted_cost_usd": 0.87
}
```

### Completion time (placement comparison)

```bash
# Compare estimated completion times across hosts
weft estimate completion \
  --command "python train.py" \
  --hosts cool30,cool100,cloud:a100

# Output:
{
  "hosts": {
    "cool30": {
      "queue_wait_s": {"median": 1200, "p90": 3600},
      "runtime_s": {"median": 3200, "p90": 5400},
      "completion_s": {"median": 4400, "p90": 7800}
    },
    "cool100": {
      "queue_wait_s": {"median": 0, "p90": 0},
      "runtime_s": {"median": 1850, "p90": 2700},
      "completion_s": {"median": 1850, "p90": 2700}
    },
    "cloud:a100": {
      "queue_wait_s": {"median": 0, "p90": 0},
      "runtime_s": {"median": 1850, "p90": 2700},
      "setup_s": {"median": 365, "p90": 780},
      "cost_usd": {"median": 0.74, "p90": 1.17},
      "completion_s": {"median": 2215, "p90": 3480}
    }
  },
  "recommended": "cool100"
}
```

### Progress estimation

```bash
# Estimate overall progress for a multi-phase job
weft estimate progress \
  --current-phase 2 \
  --phase-progress 0.65

# Output:
{
  "estimated_total_phases": 3,
  "overall_progress": 0.55,
  "confidence": "estimated"
}
```

## Integration Patterns

### SLURM scheduler plugin
A SLURM `PrologSlurmctld` or scheduling plugin could call `weft estimate duration`
to inform backfill decisions. SLURM's backfill scheduler benefits from accurate
runtime estimates — jobs with tighter bounds get backfilled more aggressively.

```bash
# In PrologSlurmctld:
EST=$(weft estimate duration --command "$SLURM_JOB_COMMAND" --gpu "$SLURM_GRES" --json)
TIMELIMIT=$(echo "$EST" | jq -r '.duration_s.p90')
```

### SkyPilot cost comparison
Before launching, SkyPilot or a wrapper script could call `weft estimate cloud-cost`
to compare expected cost across instance types, incorporating weft's survival
model and overhead estimates that SkyPilot doesn't have.

### Custom workflow managers
Any tool that submits GPU jobs can use `weft estimate` to answer:
- How long will this job take on GPU X?
- Should I run locally or burst to cloud?
- How much disk do I need for this model?
- What's the risk-adjusted cost of a spot instance?

## Data Requirements

The estimators depend on weft's local data:

| Estimator | Data source |
|---|---|
| Duration / RSS / GPU memory | `job-estimator` trained model (from weft's job history) |
| GPU scaling | Static roofline model in `internal/placement/gpugen.go` |
| Transfer bandwidth | Learned EMA in `internal/transferbw/` (from weft's transfer observations) |
| Cloud overhead | Hierarchical Bayesian model in `internal/estimate/` (from campaign history) |
| Survival probability | Beta-Binomial model in `internal/bidding/` (from campaign history) |
| Completion time | Live host metrics + queue state from `internal/placement/` |

For external tools to get useful estimates, they need either:
1. **Shared weft DB**: The tool runs on a machine with access to weft's SQLite
   database and trained models (e.g., a shared login node).
2. **Estimation server**: Weft runs as a lightweight HTTP service that external
   tools query. This avoids CLI startup overhead for frequent queries.

## Open Questions

1. **CLI vs. HTTP**: For high-frequency callers (SLURM scheduling thousands of
   jobs), CLI startup overhead may matter. Should weft also expose an HTTP
   endpoint, or is a Unix socket / long-running process sufficient?

2. **Stale models**: Weft now rebuilds stale but schema-compatible predictor
   models in the background and keeps serving the previous compatible model
   until the swap completes. Schema-incompatible models are blocked until the
   rebuild finishes. Should weft expose model freshness metadata (last retrain
   date, training set size, rebuild-in-progress)?

3. **Estimation without job history**: On a fresh install or for a SLURM cluster
   that doesn't use weft for job submission, the ML predictor has no training
   data. The roofline GPU scaling and static overhead priors still work, but
   duration prediction falls back to defaults. Is this useful enough for
   external callers?

4. **Output stability**: External tools will parse the JSON output. Should weft
   version the estimation API (e.g., `weft estimate --api-version 1`) to avoid
   breaking callers when fields change?

5. **Scope boundary**: Some estimators (completion time, placement comparison)
   depend on live host state (queue depth, current GPU utilization). Should
   these be exposed to external tools, or only the stateless estimators
   (duration, cost, transfer time)?
