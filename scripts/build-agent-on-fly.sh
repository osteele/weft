#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
    echo "Usage: $0 <version> <output-path>" >&2
    exit 1
fi

VERSION="$1"
OUTPUT_PATH="$2"

: "${WEFT_FLY_BUILDER_APP:?WEFT_FLY_BUILDER_APP not set}"
: "${WEFT_FLY_BUILDER_MACHINE:?WEFT_FLY_BUILDER_MACHINE not set}"

if [ -z "${FLY_ACCESS_TOKEN:-}" ] && [ -f "$HOME/.fly/config.yml" ]; then
    FLY_ACCESS_TOKEN="$(ruby -e 'puts File.readlines(File.expand_path("~/.fly/config.yml")).grep(/^access_token:/).first.to_s.sub(/^access_token:\s*/, "").strip')"
    export FLY_ACCESS_TOKEN
fi

REMOTE_BASE="${WEFT_FLY_BUILDER_BASE:-/data/weft-builder}"
REMOTE_OUTPUT="$REMOTE_BASE/output/weft-agent-linux-amd64"
REMOTE_WORKTREE="$REMOTE_BASE/worktree"
REMOTE_GOCACHE="$REMOTE_BASE/cache/go-build"
REMOTE_GOMODCACHE="$REMOTE_BASE/cache/gomod"
REMOTE_GO_BIN="${WEFT_FLY_BUILDER_GO_BIN:-/usr/local/go/bin/go}"
PROXY_PORT="${WEFT_FLY_BUILDER_PROXY_PORT:-2222}"
FLY_SSH_KEY="${WEFT_FLY_SSH_KEY:-$HOME/.fly/ssh/fly_ed25519}"

PROXY_PID=""
cleanup() {
    [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null || true
    flyctl machine stop "$WEFT_FLY_BUILDER_MACHINE" \
        -a "$WEFT_FLY_BUILDER_APP" \
        --wait-timeout 2m >/dev/null
}
trap cleanup EXIT

# Issue a Fly SSH certificate if needed (valid 24h; overwrite to refresh)
if [ ! -f "${FLY_SSH_KEY}" ] || [ ! -f "${FLY_SSH_KEY}-cert.pub" ]; then
    echo "Issuing Fly SSH certificate..."
    mkdir -p "$(dirname "${FLY_SSH_KEY}")"
    flyctl ssh issue personal "${FLY_SSH_KEY}" \
        --username root --hours 24 --overwrite 2>/dev/null
fi

echo "Starting Fly builder ${WEFT_FLY_BUILDER_APP}/${WEFT_FLY_BUILDER_MACHINE}..."
flyctl machine start "$WEFT_FLY_BUILDER_MACHINE" -a "$WEFT_FLY_BUILDER_APP" >/dev/null

echo "Starting SSH proxy on localhost:${PROXY_PORT}..."
flyctl proxy "${PROXY_PORT}:22" -a "$WEFT_FLY_BUILDER_APP" &
PROXY_PID=$!

FLY_SSH="ssh -p ${PROXY_PORT} -i ${FLY_SSH_KEY} -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

# Wait for proxy to be ready (poll instead of fixed sleep)
for i in $(seq 1 20); do
    ${FLY_SSH} root@localhost true 2>/dev/null && break
    sleep 1
done

echo "Preparing Fly builder..."
# Restore rsync from /data/bin (persistent volume) or install and cache it there.
# The root filesystem is ephemeral (wiped on stop/restart); /data persists.
printf '
if [ -f /data/bin/rsync ]; then
    cp /data/bin/rsync /usr/bin/rsync
    cp /data/lib/libpopt.so.0* /lib/x86_64-linux-gnu/ 2>/dev/null || true
else
    apt-get update -qq && apt-get install -y --no-install-recommends rsync
    mkdir -p /data/bin /data/lib
    cp /usr/bin/rsync /data/bin/rsync
    cp /lib/x86_64-linux-gnu/libpopt.so.0* /data/lib/
fi
mkdir -p %s %s/output %s %s
' "${REMOTE_WORKTREE}" "${REMOTE_BASE}" "${REMOTE_GOCACHE}" "${REMOTE_GOMODCACHE}" \
    | ${FLY_SSH} root@localhost /bin/bash

echo "Syncing source tree to Fly builder..."
rsync -az --delete \
    -e "${FLY_SSH}" \
    --exclude='.git/' --exclude='.jj/' --exclude='.claude/' \
    --exclude='.cache/' --exclude='.gocache/' --exclude='.gomodcache/' \
    --exclude='.bench-*-gocache/' --exclude='.bench-*-gomodcache/' \
    --exclude='testdata/' --exclude='dist/' \
    '--exclude=internal/agentdeploy/binaries/weft-agent-*' \
    '--exclude=internal/agentdeploy/binaries/VERSION' \
    '--exclude=weft' '--exclude=placement.test' \
    ./ "root@localhost:${REMOTE_WORKTREE}/"

echo "Building linux/amd64 agent on Fly..."
printf 'cd %s && GOCACHE=%s GOMODCACHE=%s CGO_ENABLED=1 GOOS=linux GOARCH=amd64 %s build -ldflags "-X main.version=%s" -o %s ./cmd/agent\n' \
    "${REMOTE_WORKTREE}" "${REMOTE_GOCACHE}" "${REMOTE_GOMODCACHE}" \
    "${REMOTE_GO_BIN}" "${VERSION}" "${REMOTE_OUTPUT}" \
    | ${FLY_SSH} root@localhost /bin/bash

mkdir -p "$(dirname "$OUTPUT_PATH")"
echo "Downloading built agent..."
rsync -az \
    -e "${FLY_SSH}" \
    "root@localhost:${REMOTE_OUTPUT}" \
    "${OUTPUT_PATH}"
