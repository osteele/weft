#!/usr/bin/env bash
# Tests for GPU class scheduling functions in queue-runner.sh
# Run: bash internal/scripts/queue-runner-gpu-class_test.sh

set -euo pipefail

PASS=0
FAIL=0

assert_eq() {
    local label="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  PASS: $label"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $label (expected '$expected', got '$actual')"
        FAIL=$((FAIL + 1))
    fi
}

assert_ok() {
    local label="$1"
    shift
    if "$@" 2>/dev/null; then
        echo "  PASS: $label"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $label (expected success)"
        FAIL=$((FAIL + 1))
    fi
}

assert_fail() {
    local label="$1"
    shift
    if ! "$@" 2>/dev/null; then
        echo "  PASS: $label"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $label (expected failure)"
        FAIL=$((FAIL + 1))
    fi
}

# --- Mock globals ---
GPU_CLASS_MAP='{"0":"NVIDIA A100-PCIE-80GB","1":"NVIDIA A100-PCIE-80GB","2":"NVIDIA GeForce RTX 2080 Ti"}'
GPU_TOTAL_MEM_GB='{"0":80,"1":80,"2":11}'
RUNNING_JSON='{}'

# --- Source the functions we need ---
# We extract just the functions to test, to avoid the full queue-runner startup
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$SCRIPT_DIR/queue-runner.sh"

# Define stubs for functions referenced by the functions under test
total_gpu_mem_reserved() {
    local device="$1"
    jq -r --arg dev "$device" '
        [.[]? | select(.gpu_devices // [] | index($dev)) | .gpu_mem_gb // 0] | add // 0
    ' <<< "$RUNNING_JSON"
}

# Extract gpu_class_devices function from the script
eval "$(sed -n '/^gpu_class_devices()/,/^}/p' "$SCRIPT")"
eval "$(sed -n '/^pick_best_gpu_for_class()/,/^}/p' "$SCRIPT")"
# PICKED_GPU is set by pick_best_gpu_for_class
PICKED_GPU=""

echo "=== gpu_class_devices ==="

result=$(gpu_class_devices "A100")
assert_eq "A100 matches devices 0 and 1" "0 1" "$result"

result=$(gpu_class_devices "2080")
assert_eq "2080 matches device 2" "2" "$result"

result=$(gpu_class_devices "nonexistent")
assert_eq "nonexistent returns empty" "" "$result"

result=$(gpu_class_devices "a100")
assert_eq "a100 (lowercase) matches devices 0 and 1" "0 1" "$result"

result=$(gpu_class_devices "NVIDIA")
assert_eq "NVIDIA matches all devices" "0 1 2" "$result"

echo ""
echo "=== pick_best_gpu_for_class ==="

RUNNING_JSON='{}'
assert_ok "A100 with 20GB, no running jobs" pick_best_gpu_for_class "A100" 20
# Both A100s have 80GB free; either 0 or 1 is valid
if [ "$PICKED_GPU" != "0" ] && [ "$PICKED_GPU" != "1" ]; then
    echo "  FAIL: expected PICKED_GPU to be 0 or 1, got '$PICKED_GPU'"
    FAIL=$((FAIL + 1))
else
    echo "  PASS: PICKED_GPU is $PICKED_GPU (valid A100 device)"
    PASS=$((PASS + 1))
fi

# Simulate 60GB reserved on device 0
RUNNING_JSON='{"99": {"gpu_devices": ["0"], "gpu_mem_gb": 60}}'
assert_ok "A100 with 20GB, 60GB reserved on device 0" pick_best_gpu_for_class "A100" 20
assert_eq "picks device 1 (more available)" "1" "$PICKED_GPU"

# Request more than any A100 has available
RUNNING_JSON='{}'
assert_fail "A100 with 90GB fails (no device has 90GB)" pick_best_gpu_for_class "A100" 90

# Request nonexistent class
assert_fail "nonexistent class fails" pick_best_gpu_for_class "V100" 10

# 2080 Ti with small request
RUNNING_JSON='{}'
assert_ok "2080 with 5GB request" pick_best_gpu_for_class "2080" 5
assert_eq "picks device 2" "2" "$PICKED_GPU"

echo ""
echo "=== Results ==="
echo "  $PASS passed, $FAIL failed"

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
