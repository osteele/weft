# Manual Testing Runbook

This document provides test procedures for verifying the weft queue runner functionality.
Tests are categorized by whether they can be run by an agent (automated) or require human intervention.

## Prerequisites

- Access to a remote host (e.g., `studio`, `titan`)
- `weft` CLI installed locally
- Network connectivity to the remote host

## Test Categories

### Automated Tests (Agent-Runnable)

These tests can be run by an agent without human intervention.

#### 1. Basic Job Queueing

**Purpose**: Verify a job can be added to the queue and runs to completion.

```bash
# Queue a simple job
weft queue add -d "Test: echo hello" studio 'echo "hello world"'

# Wait for completion and check status
weft status --wait 5

# Verify job completed
weft list --status completed --limit 1
```

**Expected**: Job transitions from `queued` -> `running` -> `completed`.

#### 2. Multiple Queued Jobs

**Purpose**: Verify multiple jobs are scheduled without getting stuck.

```bash
# Queue 3 jobs
weft queue add -d "Test: job 1" studio 'echo "job 1"; sleep 2'
weft queue add -d "Test: job 2" studio 'echo "job 2"; sleep 2'
weft queue add -d "Test: job 3" studio 'echo "job 3"; sleep 2'

# Watch them complete
weft status --wait 10

# Verify all completed
weft list --status completed --limit 3
```

**Expected**: All three jobs complete. Their execution may overlap when CPU
allotments fit the host target; FIFO governs which eligible pending job is
considered first, not completion order.

#### 3. Job Cancellation (Queued)

**Purpose**: Verify a queued job can be canceled before it runs.

```bash
# Queue a long-running job
JOB_ID=$(weft queue add -d "Test: cancel me" studio 'sleep 60' --json | jq .job_id)

# Queue another job to ensure the first is queued
weft queue add -d "Test: blocker" studio 'sleep 5'

# Cancel the first job
weft cancel $JOB_ID

# Sync and verify
weft sync
weft show $JOB_ID
```

**Expected**: Job status is `canceled`. Job never runs.

#### 4. Job Killing (Running)

**Purpose**: Verify a running job can be killed.

```bash
# Queue a long-running job
JOB_ID=$(weft queue add -d "Test: kill me" studio 'sleep 300' --json | jq .job_id)

# Wait for it to start running
sleep 5
weft sync

# Kill it
weft kill $JOB_ID

# Verify
weft sync
weft show $JOB_ID
```

**Expected**: Job status is `killed`. Process is terminated on remote.

#### 5. Job with Environment Variables

**Purpose**: Verify environment variables are passed to jobs.

```bash
# Queue job with env vars
weft queue add -d "Test: env vars" --env "FOO=bar" --env "BAZ=qux" studio 'echo "FOO=$FOO BAZ=$BAZ"'

# Wait for completion
weft status --wait 5

# Check logs for env var output
weft logs $(weft list --status completed --limit 1 --json | jq -r '.[0].id')
```

**Expected**: Logs show `FOO=bar BAZ=qux`.

#### 6. Job Description Update

**Purpose**: Verify job descriptions can be updated.

```bash
# Queue a job
JOB_ID=$(weft queue add -d "Original description" studio 'sleep 30' --json | jq .job_id)

# Update description
weft describe $JOB_ID -m "Updated description"

# Verify
weft show $JOB_ID
```

**Expected**: Description shows "Updated description".

#### 7. Queue Runner Auto-Start

**Purpose**: Verify Go agent starts automatically when jobs are queued.

```bash
# Stop queue runner if running
weft queue stop studio

# Verify stopped
ssh studio 'ps aux | grep weft-agent' | grep -v grep || echo "Agent stopped"

# Queue a job (should auto-start agent)
weft queue add -d "Test: auto-start" studio 'echo "started"'

# Verify agent started
ssh studio 'ps aux | grep weft-agent' | grep -v grep
```

**Expected**: Go agent process is running after queueing a job.

#### 8. Agent Binary Deployment

**Purpose**: Verify Go agent binary is deployed and up to date.

```bash
# Check current remote agent version
ssh studio '~/.cache/weft/bin/weft-agent --version'

# Deploy the current agent and restart the runner if needed
weft queue update studio

# Verify updated
ssh studio '~/.cache/weft/bin/weft-agent --version'
```

**Expected**: Remote agent version matches local build.

#### 9. Job with Working Directory

**Purpose**: Verify jobs run in specified working directory.

```bash
# Queue job with specific working directory
weft queue add -d "Test: workdir" -C /tmp studio 'pwd'

# Wait and check logs
weft status --wait 5
weft logs $(weft list --status completed --limit 1 --json | jq -r '.[0].id')
```

**Expected**: Logs show `/tmp`.

#### 10. Sync Detects Orphaned Jobs

**Purpose**: Verify sync correctly detects jobs no longer in remote queue.

```bash
# This test requires that no old orphaned jobs exist
# First, clean up any stale jobs
weft list --status queued --json | jq -r '.[].id' | while read id; do
  weft cancel $id 2>/dev/null || true
done
weft sync

# Queue and complete a job
weft queue add -d "Test: normal job" studio 'echo done'
weft status --wait 5

# Verify completed status was synced
weft list --status completed --limit 1
```

**Expected**: Completed job status is correctly synced from remote.

---

### Manual Tests (Human Required)

These tests require human intervention (network changes, manual process manipulation).

#### 11. Network Disconnection During Queue

**Purpose**: Verify job is queued locally when network is unavailable, then synced when restored.

**Steps**:

1. Disconnect from network (disable WiFi or VPN)
2. Run:
   ```bash
   weft queue add -d "Test: offline queue" studio 'echo offline-test'
   ```
3. Observe output - should say "queued locally"
4. Check local status:
   ```bash
   weft list --status queued
   ```
5. Reconnect to network
6. Run sync:
   ```bash
   weft sync
   ```
7. Verify job is now in remote queue:
   ```bash
   weft status
   ```

**Expected**: Job queued locally while offline, synced to remote after reconnection.

#### 12. Network Disconnection During Sync

**Purpose**: Verify sync gracefully handles network disconnection mid-operation.

**Steps**:

1. Queue several jobs
2. Start a sync while disconnecting network:
   ```bash
   weft sync & # Start in background
   # Quickly disconnect network
   ```
3. Observe error handling
4. Reconnect and re-run sync

**Expected**: Sync fails gracefully with connection error, succeeds after reconnection.

#### 13. Remote Host Reboot

**Purpose**: Verify jobs recover after remote host reboots.

**Steps**:

1. Queue a long-running job:
   ```bash
   weft queue add -d "Test: reboot recovery" studio 'sleep 3600'
   ```
2. Verify job is running:
   ```bash
   weft sync
   weft list --status running
   ```
3. Reboot the remote host (requires SSH access):
   ```bash
   ssh studio 'sudo reboot'
   ```
4. Wait for host to come back online
5. Run sync:
   ```bash
   weft sync
   ```
6. Check job status:
   ```bash
   weft list
   ```

**Expected**: Job is marked as `dead` (process no longer running after reboot).

#### 14. Agent Crash Recovery

**Purpose**: Verify system recovers if the Go agent crashes.

**Steps**:

1. Queue a job
2. Find and kill the agent process:
   ```bash
   ssh studio 'pkill -f weft-agent'
   ```
3. Queue another job:
   ```bash
   weft queue add -d "Test: recovery" studio 'echo recovered'
   ```
4. Verify runner restarts and jobs complete

**Expected**: Queue runner restarts automatically, pending jobs run.

---

## Debugging Commands

### Check Queue Runner Logs

```bash
ssh studio 'tail -50 ~/.cache/weft/queue/runner-default.log'
```

### Check Queue State

```bash
# Current running job
ssh studio 'cat ~/.cache/weft/queue/default.state.json'

# Pending commands
ssh studio 'cat ~/.cache/weft/queue/default.commands'
```

### Check If Agent Is Alive

```bash
ssh studio 'ps aux | grep weft-agent | grep -v grep'
```

### Check Job Logs

```bash
# List log files
ssh studio 'ls -la ~/.cache/weft/logs/'

# Tail specific job log
ssh studio 'tail -30 ~/.cache/weft/logs/<job-id>-*.log'
```

### Check Local Operations Log

```bash
tail -50 ~/.cache/weft/oplog.jsonl
```

### Check Remote Operations Log

```bash
ssh studio 'tail -50 ~/.cache/weft/queue/oplog.jsonl'
```

---

## Campaign Testing with Testdata Projects

Test projects live in `testdata/campaign/` with pre-configured Python environments:

| Project | Purpose | Duration | Exit |
|---------|---------|----------|------|
| `basic` | GPU detection, output files, progress reporting (torch, numpy) | ~20s | 0 |
| `ml-tokenizer` | HuggingFace tokenizer download + inference | ~20-45s | 0 |
| `ml-inference` | DistilBERT model GPU inference | ~30-60s | 0 |
| `fail` | Deliberate `RuntimeError` after 2s sleep | ~2s | non-zero |

Each has `pyproject.toml`, `run.py`, `.venv/`, and `uv.lock`.

### Step 1: Queue testdata jobs

Use `--tag rental` to skip local placement and force jobs to be unplaced (requiring a rental GPU).
Also specify `--gpu turing+` to require at least a Turing-generation GPU:

```bash
weft run --gpu turing+ --tag rental --tag test-campaign -C "$(pwd)/testdata/campaign/basic" 'uv run python run.py'
weft run --gpu turing+ --tag rental --tag test-campaign -C "$(pwd)/testdata/campaign/ml-tokenizer" 'uv run python run.py'
weft run --gpu turing+ --tag rental --tag test-campaign -C "$(pwd)/testdata/campaign/ml-inference" 'uv run python run.py'
weft run --gpu turing+ --tag rental --tag test-campaign -C "$(pwd)/testdata/campaign/fail" 'uv run python run.py'
```

### Step 2: Preview and launch

```bash
# Dry run to check grouping and cost estimate
weft instance launch --jobs <ids> --max-spend '$1.00' --max-time 30m --dry-run

# Launch with short grace period for faster testing
weft instance launch --jobs <ids> --max-spend '$1.00' --max-time 30m --grace-period 2m --yes --no-watch
```

### Step 3: Monitor

```bash
weft campaign watch <campaign-id> --plain
# or check DB directly
sqlite3 ~/.local/state/weft/jobs.db "SELECT id, status, gpu_class FROM jobs WHERE id IN (<ids>);"
```

### Step 4: Verify telemetry and phases

After jobs complete, check the remote log directory for each job:

```bash
# SSH into the cloud instance (or use weft instance ssh <id>)
# Check phases.json — setup should be separate from run
cat ~/.cache/weft/logs/<job-id>.phases.json | python3 -m json.tool

# Check timeseries — should have at least one sample even for short jobs
cat ~/.cache/weft/logs/<job-id>.timeseries.jsonl

# Check completion record
cat ~/.cache/weft/logs/<job-id>.completion.json | python3 -m json.tool
```

**What to verify:**

- **Phase timing**: `setup_start` < `setup_end` ≤ `run_start` (setup runs `uv sync` as a separate process)
- **Setup seconds**: `setup_seconds` > 0 when `uv sync` actually ran (projects with `pyproject.toml` + `uv.lock` or `.venv/`)
- **GPU metrics**: `timeseries.jsonl` has at least one sample (immediate sample before ticker)
- **GPU fields**: `gpu_mem_used`, `gpu_util_pct` are non-null in timeseries samples
- **Failure detection**: The `fail` project should have non-zero exit code and a failure reason

### Step 5: Verify log retrieval

After instances terminate, logs should still be accessible from the local cache or R2:

```bash
# Logs should be accessible even after instance termination
weft log <job-id>
# Should show full job output, not SSH errors

# Force sync to ensure logs are cached
weft sync
weft log <job-id>
```

**What to verify:**
- No SSH errors for cloud jobs
- Log content matches what the job actually printed

### Step 6: Verify artifact retrieval

```bash
# List discovered output files (reads from R2)
weft artifact list <job-id>
# Should show output files if the job wrote to output/ or outputs/

# Sync outputs to local working directory (downloads from R2)
weft artifact sync <job-id>
# Should download output files from R2 to the local project directory
```

**What to verify:**
- `artifact list` shows output files for jobs that wrote to `output/` or `outputs/`
- `artifact sync` downloads the files locally
- The `basic` testdata project writes to `output/`, so it should have artifacts

### Step 7: Verify progress reporting

```bash
# While the basic job is running, campaign watch should show progress:
weft campaign watch <campaign-id> --plain
# Expected output includes lines like:
#   instance <id>: job <job-id> progress: 50%

# TUI mode shows progress inline:
weft campaign watch <campaign-id>
# Running jobs should display "running  50%" instead of just "running"
```

**What to verify:**

- The `basic` testdata job shows progress updates during `campaign watch --plain`
- Progress percentage increases over the ~20s run
- After the job completes, progress lines stop (the R2 key is cleaned up)
- Jobs that don't emit progress lines (like `ml-tokenizer`) show plain "running" with no percentage

### Re-running test jobs

To reset completed test jobs for re-testing:

```bash
sqlite3 ~/.local/state/weft/jobs.db "UPDATE jobs SET status='queued', host='' WHERE id IN (<ids>);"
```

---

## Cleanup

After testing, clean up test jobs:

```bash
# Cancel all queued jobs
weft list --status queued --json | jq -r '.[].id' | while read id; do
  weft cancel $id 2>/dev/null || true
done

# Kill any running test jobs
weft list --status running --json | jq -r '.[].id' | while read id; do
  weft kill $id 2>/dev/null || true
done

# Sync to update statuses
weft sync
```
