# Manual Testing Runbook

This document provides test procedures for verifying the remote-jobs queue runner functionality.
Tests are categorized by whether they can be run by an agent (automated) or require human intervention.

## Prerequisites

- Access to a remote host (e.g., `studio`, `cool30`)
- `remote-jobs` CLI installed locally
- Network connectivity to the remote host

## Test Categories

### Automated Tests (Agent-Runnable)

These tests can be run by an agent without human intervention.

#### 1. Basic Job Queueing

**Purpose**: Verify a job can be added to the queue and runs to completion.

```bash
# Queue a simple job
remote-jobs queue add -d "Test: echo hello" studio 'echo "hello world"'

# Wait for completion and check status
remote-jobs status --wait 5

# Verify job completed
remote-jobs list --status completed --limit 1
```

**Expected**: Job transitions from `queued` -> `running` -> `completed`.

#### 2. Multiple Sequential Jobs

**Purpose**: Verify multiple jobs run in sequence without getting stuck.

```bash
# Queue 3 jobs
remote-jobs queue add -d "Test: job 1" studio 'echo "job 1"; sleep 2'
remote-jobs queue add -d "Test: job 2" studio 'echo "job 2"; sleep 2'
remote-jobs queue add -d "Test: job 3" studio 'echo "job 3"; sleep 2'

# Watch them complete
remote-jobs status --wait 10

# Verify all completed
remote-jobs list --status completed --limit 3
```

**Expected**: All 3 jobs complete in order. No jobs get stuck in `queued` state.

#### 3. Job Cancellation (Queued)

**Purpose**: Verify a queued job can be canceled before it runs.

```bash
# Queue a long-running job
JOB_ID=$(remote-jobs queue add -d "Test: cancel me" studio 'sleep 60' --json | jq .job_id)

# Queue another job to ensure the first is queued
remote-jobs queue add -d "Test: blocker" studio 'sleep 5'

# Cancel the first job
remote-jobs cancel $JOB_ID

# Sync and verify
remote-jobs sync
remote-jobs show $JOB_ID
```

**Expected**: Job status is `canceled`. Job never runs.

#### 4. Job Killing (Running)

**Purpose**: Verify a running job can be killed.

```bash
# Queue a long-running job
JOB_ID=$(remote-jobs queue add -d "Test: kill me" studio 'sleep 300' --json | jq .job_id)

# Wait for it to start running
sleep 5
remote-jobs sync

# Kill it
remote-jobs kill $JOB_ID

# Verify
remote-jobs sync
remote-jobs show $JOB_ID
```

**Expected**: Job status is `killed`. Process is terminated on remote.

#### 5. Job with Environment Variables

**Purpose**: Verify environment variables are passed to jobs.

```bash
# Queue job with env vars
remote-jobs queue add -d "Test: env vars" --env "FOO=bar" --env "BAZ=qux" studio 'echo "FOO=$FOO BAZ=$BAZ"'

# Wait for completion
remote-jobs status --wait 5

# Check logs for env var output
remote-jobs logs $(remote-jobs list --status completed --limit 1 --json | jq -r '.[0].id')
```

**Expected**: Logs show `FOO=bar BAZ=qux`.

#### 6. Job Description Update

**Purpose**: Verify job descriptions can be updated.

```bash
# Queue a job
JOB_ID=$(remote-jobs queue add -d "Original description" studio 'sleep 30' --json | jq .job_id)

# Update description
remote-jobs describe $JOB_ID -m "Updated description"

# Verify
remote-jobs show $JOB_ID
```

**Expected**: Description shows "Updated description".

#### 7. Queue Runner Auto-Start

**Purpose**: Verify queue runner starts automatically when jobs are queued.

```bash
# Stop queue runner if running
remote-jobs queue stop studio

# Verify stopped
ssh studio 'ps aux | grep queue-runner' | grep -v grep || echo "Runner stopped"

# Queue a job (should auto-start runner)
remote-jobs queue add -d "Test: auto-start" studio 'echo "started"'

# Verify runner started
ssh studio 'ps aux | grep queue-runner' | grep -v grep
```

**Expected**: Queue runner process is running after queueing a job.

#### 8. Script Build Number Upgrade

**Purpose**: Verify queue runner script is upgraded when build number increases.

```bash
# Check current remote script build number
ssh studio 'grep "# BUILD:" ~/.cache/remote-jobs/bin/queue-runner.sh | head -1'

# View local build number
grep "# BUILD:" internal/scripts/queue-runner.sh | head -1

# Install with force-upgrade
remote-jobs queue start --install studio

# Verify updated
ssh studio 'grep "# BUILD:" ~/.cache/remote-jobs/bin/queue-runner.sh | head -1'
```

**Expected**: Remote script build number matches local.

#### 9. Job with Working Directory

**Purpose**: Verify jobs run in specified working directory.

```bash
# Queue job with specific working directory
remote-jobs queue add -d "Test: workdir" -C /tmp studio 'pwd'

# Wait and check logs
remote-jobs status --wait 5
remote-jobs logs $(remote-jobs list --status completed --limit 1 --json | jq -r '.[0].id')
```

**Expected**: Logs show `/tmp`.

#### 10. Sync Detects Orphaned Jobs

**Purpose**: Verify sync correctly detects jobs no longer in remote queue.

```bash
# This test requires that no old orphaned jobs exist
# First, clean up any stale jobs
remote-jobs list --status queued --json | jq -r '.[].id' | while read id; do
  remote-jobs cancel $id 2>/dev/null || true
done
remote-jobs sync

# Queue and complete a job
remote-jobs queue add -d "Test: normal job" studio 'echo done'
remote-jobs status --wait 5

# Verify completed status was synced
remote-jobs list --status completed --limit 1
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
   remote-jobs queue add -d "Test: offline queue" studio 'echo offline-test'
   ```
3. Observe output - should say "queued locally"
4. Check local status:
   ```bash
   remote-jobs list --status queued
   ```
5. Reconnect to network
6. Run sync:
   ```bash
   remote-jobs sync
   ```
7. Verify job is now in remote queue:
   ```bash
   remote-jobs status
   ```

**Expected**: Job queued locally while offline, synced to remote after reconnection.

#### 12. Network Disconnection During Sync

**Purpose**: Verify sync gracefully handles network disconnection mid-operation.

**Steps**:

1. Queue several jobs
2. Start a sync while disconnecting network:
   ```bash
   remote-jobs sync & # Start in background
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
   remote-jobs queue add -d "Test: reboot recovery" studio 'sleep 3600'
   ```
2. Verify job is running:
   ```bash
   remote-jobs sync
   remote-jobs list --status running
   ```
3. Reboot the remote host (requires SSH access):
   ```bash
   ssh studio 'sudo reboot'
   ```
4. Wait for host to come back online
5. Run sync:
   ```bash
   remote-jobs sync
   ```
6. Check job status:
   ```bash
   remote-jobs list
   ```

**Expected**: Job is marked as `dead` (process no longer running after reboot).

#### 14. Queue Runner Crash Recovery

**Purpose**: Verify system recovers if queue runner crashes.

**Steps**:

1. Queue a job
2. Find and kill the queue runner process:
   ```bash
   ssh studio 'pkill -f queue-runner'
   ```
3. Queue another job:
   ```bash
   remote-jobs queue add -d "Test: recovery" studio 'echo recovered'
   ```
4. Verify runner restarts and jobs complete

**Expected**: Queue runner restarts automatically, pending jobs run.

---

## Debugging Commands

### Check Queue Runner Logs

```bash
ssh studio 'tail -50 ~/.cache/remote-jobs/queue/runner-default.log'
```

### Check Queue State

```bash
# Current running job
ssh studio 'cat ~/.cache/remote-jobs/queue/default.state.json'

# Pending commands
ssh studio 'cat ~/.cache/remote-jobs/queue/default.commands'
```

### Check If Runner Is Alive

```bash
ssh studio 'ps aux | grep queue-runner | grep -v grep'
```

### Check Job Logs

```bash
# List log files
ssh studio 'ls -la ~/.cache/remote-jobs/logs/'

# Tail specific job log
ssh studio 'tail -30 ~/.cache/remote-jobs/logs/<job-id>-*.log'
```

### Check Local Operations Log

```bash
tail -50 ~/.cache/remote-jobs/oplog.jsonl
```

### Check Remote Operations Log

```bash
ssh studio 'tail -50 ~/.cache/remote-jobs/queue/oplog.jsonl'
```

---

## Cleanup

After testing, clean up test jobs:

```bash
# Cancel all queued jobs
remote-jobs list --status queued --json | jq -r '.[].id' | while read id; do
  remote-jobs cancel $id 2>/dev/null || true
done

# Kill any running test jobs
remote-jobs list --status running --json | jq -r '.[].id' | while read id; do
  remote-jobs kill $id 2>/dev/null || true
done

# Sync to update statuses
remote-jobs sync
```
