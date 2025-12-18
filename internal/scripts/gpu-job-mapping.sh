#!/bin/bash
#
# Map jobs to GPUs by checking process trees
# Input: space-separated list of "job_id:pid_file" pairs
# Output: JOB_GPU:job_id:gpu_index:mem_mib lines
#

set -euo pipefail

# Get all GPU processes once
GPU_PROCS=$(nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory \
  --format=csv,noheader,nounits 2>/dev/null || true)

if [ -z "$GPU_PROCS" ]; then
  # No GPU processes or no nvidia-smi
  exit 0
fi

# Get GPU index→UUID mapping
GPU_UUIDS=$(nvidia-smi --query-gpu=index,uuid --format=csv,noheader 2>/dev/null || true)

# Function to get all descendant PIDs recursively
get_descendants() {
  local pid=$1
  local children
  children=$(pgrep -P "$pid" 2>/dev/null || true)
  echo "$children"
  for child in $children; do
    get_descendants "$child"
  done
}

# For each job, get PID and descendants, match against GPU processes
for arg in "$@"; do
  JOB_ID="${arg%%:*}"
  PID_FILE="${arg#*:}"

  # Expand tilde
  PID_FILE="${PID_FILE/#\~/$HOME}"

  PID=$(cat "$PID_FILE" 2>/dev/null || true)
  [ -z "$PID" ] && continue

  # Check if main process is still running
  if ! kill -0 "$PID" 2>/dev/null; then
    continue
  fi

  # Get all descendant PIDs (recursive)
  DESCENDANTS=$(get_descendants "$PID")
  ALL_PIDS="$PID $DESCENDANTS"

  # Check each GPU process
  echo "$GPU_PROCS" | while IFS=, read -r GPID UUID MEM; do
    GPID=$(echo "$GPID" | tr -d ' ')
    [ -z "$GPID" ] && continue

    for P in $ALL_PIDS; do
      if [ "$GPID" = "$P" ]; then
        # Find GPU index from UUID
        UUID_TRIMMED=$(echo "$UUID" | tr -d ' ')
        IDX=$(echo "$GPU_UUIDS" | grep "$UUID_TRIMMED" | cut -d, -f1 | tr -d ' ')
        MEM=$(echo "$MEM" | tr -d ' ')
        echo "JOB_GPU:$JOB_ID:$IDX:$MEM"
        break  # Found match for this GPU process, move to next
      fi
    done
  done
done
