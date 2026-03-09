# Placement Telemetry

Weft records placement decision context so that scoring weights can be
optimized offline by replaying historical decisions against actual outcomes.

## What Is Recorded

Three categories of data fill the gaps between "which host was chosen" and
"was that the best choice":

### 1. `placement.decided` oplog entries

Logged whenever auto-placement selects a host (in `cmd/run.go` and
`internal/coordinator/dispatch.go`).

```jsonl
{"t":"2026-03-04T10:00:00Z","op":"placement.decided","host":"titan","detail":"selected=titan scores=titan:8.2,atlas:5.1 metrics=titan:{cpu:5,gpu:0,q:0},atlas:{cpu:80,gpu:95,q:4}"}
```

Fields in `detail`:
- `selected=<host>` — the winning host
- `scores=<host>:<score>,...` — all host scores (ineligible marked with `(x)`)
- `metrics=<host>:{cpu:N,gpu:N,q:N},...` — live metrics snapshot (when available)

### 2. `host.metrics` oplog entries

Logged after `CollectMetrics` probes hosts (in `internal/placement/metrics.go`).
One entry per host that responded.

```jsonl
{"t":"2026-03-04T10:00:00Z","op":"host.metrics","host":"atlas","detail":"cpu=80 gpu=95 ram=39 q=4 gpu_free=0:11920,1:76920"}
```

Fields in `detail`:
- `cpu=N` — CPU utilization percent
- `gpu=N` — GPU utilization percent (max across devices)
- `ram=N` — RAM utilization percent
- `q=N` — queue depth (pending jobs)
- `gpu_free=<idx>:<MiB>,...` — per-device free GPU memory

### 3. `placement_meta` on job records

Stored as JSON TEXT on the `jobs` table when auto-placement is used.

```sql
SELECT id, host, placement_meta FROM jobs WHERE placement_meta IS NOT NULL ORDER BY id DESC LIMIT 5;
```

Schema (`PlacementMeta`):

| Field | JSON key | Description |
|-------|----------|-------------|
| PredictedDurationS | `pred_dur_s` | Predicted wall-clock seconds for selected host |
| PredictedRSSKB | `pred_rss_kb` | Predicted peak RSS in KB |
| PredictedGPUMemMiB | `pred_gpu_mib` | Predicted peak GPU memory in MiB |
| SelectedScore | `score` | Placement score of the selected host |
| RunnerUpHost | `runner_up` | Second-best eligible host |
| RunnerUpScore | `runner_up_score` | Score of the runner-up host |

## Example Queries

### Oplog (jq)

```bash
# Recent placement decisions
tail -100 ~/.cache/weft/operations.log | jq 'select(.op == "placement.decided")'

# Host metrics time series for atlas
cat ~/.cache/weft/operations.log | jq 'select(.op == "host.metrics" and .host == "atlas")'

# Placement decisions where runner-up scored close to winner
cat ~/.cache/weft/operations.log | jq 'select(.op == "placement.decided") | .detail'
```

### SQLite (placement_meta)

```sql
-- Jobs with predictions and their actual durations
SELECT id, host,
  json_extract(placement_meta, '$.pred_dur_s') AS predicted_s,
  (end_time - start_time) AS actual_s,
  json_extract(placement_meta, '$.score') AS score,
  json_extract(placement_meta, '$.runner_up') AS runner_up
FROM jobs
WHERE placement_meta IS NOT NULL AND end_time IS NOT NULL
ORDER BY id DESC LIMIT 20;

-- Prediction error distribution
SELECT host,
  AVG(ABS(json_extract(placement_meta, '$.pred_dur_s') - (end_time - start_time))) AS mean_abs_error_s,
  COUNT(*) AS n
FROM jobs
WHERE placement_meta IS NOT NULL AND end_time IS NOT NULL
  AND json_extract(placement_meta, '$.pred_dur_s') IS NOT NULL
GROUP BY host;
```

## How This Enables Weight Optimization

With placement telemetry, an offline analysis can:

1. **Compute prediction error** by comparing `pred_dur_s` against actual
   `(end_time - start_time)` for each job.
2. **Replay decisions** using the recorded scores and metrics to ask: "given
   these metrics, would different scoring weights have chosen a faster host?"
3. **Identify systematic biases** such as a host that consistently finishes
   faster than predicted, suggesting its performance factor needs adjustment.
