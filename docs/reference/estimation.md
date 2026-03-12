# Estimation and Modeling

This document describes how weft estimates job runtime, resource usage,
placement suitability, transfer time, cloud campaign cost, and cloud-instance
survival risk.

## What We Estimate

Weft has several estimation tasks that feed different product behaviors:

- `weft predict` estimates runtime, peak RSS, and peak GPU memory for a single command.
- Automatic placement uses those estimates to rule hosts in or out, then score the eligible ones.
- Campaign launch estimates full wall-clock time and total cloud cost per GPU group.
- Transfer tracking estimates data-movement time between sources and destinations.
- Cloud bidding estimates whether a cheaper offer is likely to survive long enough to finish the work.

The code is split across:

- `internal/predictor` for command-level ML predictions
- `internal/placement` for placement scoring and Monte Carlo completion modeling
- `internal/estimate` for campaign runtime and overhead estimation
- `internal/transferbw` for learned transfer rates
- `internal/bidding` for cloud survival and risk-adjusted cost

## Data Sources

### Completed job runs

`weft export training-data` exports per-run JSONL records with:

- command, project, host, GPU class, backend, and tenant
- start/end times and total duration
- peak RSS, peak GPU memory, and mean CPU usage
- host hardware metadata
- time-series telemetry sampled during execution

This is the training feed for the external `job-estimator` project that weft
invokes through `uv`.

### Cloud phase telemetry

Cloud jobs write structured phase timing and cache-state metadata that land in
`job_phase_timings` and `cloud_instances`. These records support:

- startup estimation from instance creation to readiness
- SSH setup estimation from readiness to wrapper start
- job setup estimation from setup start to setup end
- upload estimation from upload start to upload end
- cold-vs-warm cache distinctions

### Transfer observations

Observed transfers are stored in `transfer_observations` with bytes moved,
duration, and endpoint provenance. Endpoints are grouped as:

- `hf`
- `r2`
- `cloud:<provider>:<datacenter>`
- `onprem:<hostname>`

### Cloud instance outcomes

Campaign history records per-instance termination outcomes, price, resolved GPU
name, and provider reliability. These are used to train the cloud survival
model.

## Command-Level Runtime And Resource Prediction

`weft predict` and placement-time prediction use the external `job-estimator`
Python project, invoked by weft through:

```bash
uv run --project <predictor.project_path> job-estimator predict ...
uv run --project <predictor.project_path> job-estimator predict-batch ...
uv run --project <predictor.project_path> job-estimator train ...
```

The predictor is configured in `~/.config/weft/config.toml`:

```toml
[predictor]
project_path = "/path/to/job-estimator"
model_dir = "~/.cache/weft/models"
retrain_interval = 50
db_paths = ["/path/to/extra/jobs.db"]
```

### Inputs

The predictor receives:

- the command string
- the project name
- the target host or GPU class
- training examples exported from one or more job databases

### Outputs

For each target, the predictor returns:

- `duration_s`
- `peak_rss_kb`
- `max_gpu_mem_mib`

Each prediction includes:

- mean
- standard deviation
- lower bound
- upper bound

Within weft, those bounds are treated conservatively for capacity checks and as
uncertainty intervals for downstream scoring.

### Retraining

Models live in `~/.cache/weft/models/`. Weft checks the model metadata and
re-trains when enough new completed jobs have accumulated, or immediately when
you run:

```bash
weft retrain
```

## Placement Scoring

Placement starts with inventory- and metrics-based eligibility, then applies
predictor outputs.

### Hard constraints

Weft marks a host ineligible when the prediction's upper bound exceeds host
capacity:

- peak RSS upper bound > host RAM
- peak GPU memory upper bound > largest GPU on the host

This is intentionally conservative: it prefers false negatives over launching a
job that is likely to OOM.

### Soft constraints

When live metrics are available, weft penalizes hosts whose free RAM or free GPU
memory would leave less than about 20% headroom relative to the predicted need.
This keeps a host eligible while expressing "it fits, but barely."

### Monte Carlo completion-time model

When at least two eligible hosts have duration predictions, weft switches from a
simple relative-duration score to a Monte Carlo completion-time model.

For each host, the simulator:

1. Fits a log-normal distribution to the predicted job duration.
2. Fits another log-normal distribution for queued jobs on that host.
3. Draws many samples of queue wait plus runtime.
4. Compares hosts by median completion time.

The queue-wait model treats the host's current queue depth as a sum of
independent queued-job durations. Bounds from the predictor are mapped into a
log-normal distribution; when bounds are unavailable, weft falls back to a
coefficient-of-variation assumption.

This is the main data-science idea in placement: choose the host with the best
predicted time-to-finish, not merely the best standalone runtime.

## Cloud Campaign Runtime And Cost Estimation

Campaign cost estimation models the entire lifecycle of a cloud run, not just
the training command.

The estimate is the sum of:

- startup
- SSH setup
- provisioning
- job setup
- run time
- upload

### Runtime phase model

The phase model uses `internal/estimate.Estimate`, which stores:

- mean duration
- lower bound
- upper bound

Those phase estimates are added together to produce a total duration interval.

### Startup, SSH setup, job setup, and upload

These phases use a hierarchical empirical-Bayes log-normal model trained from
historical cloud observations.

The model:

- works in log-duration space
- learns a global prior across all observations
- learns group-specific sufficient statistics
- shrinks sparse groups back toward the global prior

Grouping depends on the phase:

- startup: data center
- SSH setup: download-bandwidth bucket
- job setup: cold vs warm cache
- upload: upload-bandwidth bucket

This is useful because cloud overhead varies by environment, but many groups are
data-sparse. Partial pooling gives more stable estimates than fitting each group
independently.

### Provisioning

Provisioning time is estimated from transfer size and bandwidth:

- workdir sync size, with a fallback default when unknown
- Hugging Face model download bytes
- cold `uv sync` bytes

The size estimates come from:

- declared input assets resolved through `internal/dataloc`
- cached `uv` manifests stored in R2 and keyed by `uv.lock` hash

Transfer time is then estimated as:

```text
time = bytes / bandwidth
```

with wide lower and upper bounds to account for bandwidth variability.

### Run time

Run time comes from batch prediction through `job-estimator`. If no model is
configured, weft falls back to a default runtime of 1 hour per job, with a wide
uncertainty interval.

### Cost

Total cost is:

```text
estimated hours * offer price per hour
```

Campaign launch also derives generous spend and time budgets from the estimate,
using a large safety multiplier so imperfect estimates do not kill good runs.

## Transfer Bandwidth Learning

Transfer bandwidth is learned with an exponential moving average (EMA) over
observed transfers.

The current model uses:

- key: `(source_endpoint, dest_endpoint)`
- smoothing factor: `alpha = 0.3`

Recent observations therefore matter more than older ones, while still letting
the estimate stabilize after repeated transfers.

Weft uses these learned rates for:

- transfer-aware placement
- campaign provisioning and cost estimation
- future transfer analysis via stored raw observations

The raw observations are kept, not just the EMA, so the grouping key can be
refined later without losing history.

## Cloud Survival And Risk-Adjusted Cost

Cheapest-per-hour is not always cheapest-to-completion. Spot-like cloud offers
can fail or be reclaimed before the job finishes.

Weft addresses this with a Beta-Binomial survival model.

### Grouping

Outcomes are bucketed by:

- GPU family
- relative price bucket within that family

Price buckets are percentile-based: low, medium, high, premium.

### Prior and shrinkage

The model uses:

- an informative prior from the provider's reported reliability
- global historical survival as a backstop
- shrinkage toward the global rate when a group has few observations

This is another partial-pooling idea: sparse buckets should not overfit a small
number of wins or failures.

### Expected cost with retries

Weft scores offers by expected cost, not hourly price:

```text
E[cost] = job_cost / p + retry_setup_cost
```

where `p` is the estimated survival probability. The model therefore prefers an
offer that is slightly more expensive per hour when it is much more likely to
finish without forcing a restart.

## Uncertainty And Safety Margins

Weft generally prefers conservative estimates:

- upper bounds drive hard resource checks
- soft-fit penalties activate before a host is truly full
- fallback durations are intentionally wide
- budget limits derived from campaign estimates are inflated substantially

This bias is deliberate. A scheduler is more useful when it avoids obviously bad
placements and under-budgeting, even if that means occasionally passing on a
host that might have worked.

## Commands

```bash
weft predict --host atlas 'python train.py'
weft retrain
weft export training-data --output training-data.jsonl
weft campaign launch --dry-run
```

## Related Documents

- [Workflow Guide](../guides/workflow-guide.md)
- [Campaigns](../guides/campaigns.md)
- [Architecture](../design/architecture.md)
- [Coordinator Architecture](../design/coordinator-architecture.md)
- [Placement Telemetry](../design/placement-telemetry.md)
- [Future Ideas](../planning/IDEAS.md)
