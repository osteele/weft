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

# Optional: when set, use zig cc to target a specific glibc version.
# E.g. WEFT_FLY_BUILDER_ZIG_TARGET=x86_64-linux-gnu.2.31
ZIG_TARGET="${WEFT_FLY_BUILDER_ZIG_TARGET:-}"
ZIG_VERSION="${WEFT_FLY_BUILDER_ZIG_VERSION:-0.13.0}"

# rsync remote-shell wrapper: uses flyctl ssh console instead of raw SSH,
# authenticating via FLY_ACCESS_TOKEN. Written to a temp file so rsync can
# exec it via -e. rsync calls: rsh [opts] [user@]host remote-cmd...
TMPDIR_RSH="$(mktemp -d)"
FLY_RSH="$TMPDIR_RSH/fly-rsh.sh"
cat > "$FLY_RSH" << EOF
#!/bin/bash
while [[ \$# -gt 0 && "\$1" == -* ]]; do shift; done  # skip ssh-style flags
shift  # skip [user@]host
exec flyctl ssh console \
    -a "${WEFT_FLY_BUILDER_APP}" --machine "${WEFT_FLY_BUILDER_MACHINE}" -q \
    -C "\$*"
EOF
chmod +x "$FLY_RSH"

cleanup() {
    rm -rf "$TMPDIR_RSH"
    flyctl machine stop "$WEFT_FLY_BUILDER_MACHINE" \
        -a "$WEFT_FLY_BUILDER_APP" \
        --wait-timeout 2m >/dev/null
}
trap cleanup EXIT

FLY_CONSOLE="flyctl ssh console -a ${WEFT_FLY_BUILDER_APP} --machine ${WEFT_FLY_BUILDER_MACHINE} -q"

echo "Starting Fly builder ${WEFT_FLY_BUILDER_APP}/${WEFT_FLY_BUILDER_MACHINE}..."
flyctl machine start "$WEFT_FLY_BUILDER_MACHINE" -a "$WEFT_FLY_BUILDER_APP" >/dev/null

echo "Preparing Fly builder..."
# Restore tools from /data/bin (persistent volume) or install and cache them there.
# The root filesystem is ephemeral (wiped on stop/restart); /data persists.
printf '
mkdir -p /data/bin /data/lib
NEED_APT=0
if [ -f /data/bin/rsync ]; then
    cp /data/bin/rsync /usr/bin/rsync
    cp /data/lib/libpopt.so.0* /lib/x86_64-linux-gnu/ 2>/dev/null || true
else
    NEED_APT=1
fi
if [ -f /data/bin/xz ]; then
    cp /data/bin/xz /usr/bin/xz
else
    NEED_APT=1
fi
if [ "$NEED_APT" = "1" ]; then
    apt-get update -qq && apt-get install -y --no-install-recommends rsync xz-utils
    cp /usr/bin/rsync /data/bin/rsync
    cp /lib/x86_64-linux-gnu/libpopt.so.0* /data/lib/
    cp /usr/bin/xz /data/bin/xz
fi
mkdir -p %s %s/output %s %s
' "${REMOTE_WORKTREE}" "${REMOTE_BASE}" "${REMOTE_GOCACHE}" "${REMOTE_GOMODCACHE}" \
    | ${FLY_CONSOLE} -C "bash -s"

# Install zig if ZIG_TARGET is requested.
# Cache the zig binary on /data so subsequent builds skip the download.
if [ -n "${ZIG_TARGET}" ]; then
    echo "Ensuring zig ${ZIG_VERSION} is available on builder..."
    printf '
ZIG_VERSION=%s
ZIG_DIR=/data/zig-${ZIG_VERSION}
if [ ! -x "${ZIG_DIR}/zig" ]; then
    echo "Installing zig ${ZIG_VERSION}..."
    mkdir -p /data
    curl -fsSL "https://ziglang.org/download/${ZIG_VERSION}/zig-linux-x86_64-${ZIG_VERSION}.tar.xz" \
        | tar -xJ -C /data
    mv /data/zig-linux-x86_64-${ZIG_VERSION} ${ZIG_DIR}
fi
echo "zig ok: $(${ZIG_DIR}/zig version)"
' "${ZIG_VERSION}" | ${FLY_CONSOLE} -C "bash -s"
fi

echo "Syncing source tree to Fly builder..."
rsync -az --delete \
    -e "${FLY_RSH}" \
    --exclude='.git/' --exclude='.jj/' --exclude='.claude/' \
    --exclude='.cache/' --exclude='.gocache/' --exclude='.gomodcache/' \
    --exclude='.bench-*-gocache/' --exclude='.bench-*-gomodcache/' \
    --exclude='testdata/' --exclude='dist/' \
    '--exclude=internal/agentdeploy/binaries/weft-agent-*' \
    '--exclude=internal/agentdeploy/binaries/VERSION' \
    '--exclude=weft' '--exclude=placement.test' \
    ./ "placeholder:${REMOTE_WORKTREE}/"

if [ -n "${ZIG_TARGET}" ]; then
    echo "Building linux/amd64 agent on Fly (zig cc -target ${ZIG_TARGET})..."
    printf 'set -euo pipefail; ZIG_DIR=/data/zig-%s; cd %s && GOCACHE=%s GOMODCACHE=%s CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="${ZIG_DIR}/zig cc -target %s" %s build -ldflags "-X main.version=%s" -o %s ./cmd/agent\n' \
        "${ZIG_VERSION}" "${REMOTE_WORKTREE}" "${REMOTE_GOCACHE}" "${REMOTE_GOMODCACHE}" \
        "${ZIG_TARGET}" "${REMOTE_GO_BIN}" "${VERSION}" "${REMOTE_OUTPUT}" \
        | ${FLY_CONSOLE} -C "bash -s"
else
    echo "Building linux/amd64 agent on Fly..."
    printf 'cd %s && GOCACHE=%s GOMODCACHE=%s CGO_ENABLED=1 GOOS=linux GOARCH=amd64 %s build -ldflags "-X main.version=%s" -o %s ./cmd/agent\n' \
        "${REMOTE_WORKTREE}" "${REMOTE_GOCACHE}" "${REMOTE_GOMODCACHE}" \
        "${REMOTE_GO_BIN}" "${VERSION}" "${REMOTE_OUTPUT}" \
        | ${FLY_CONSOLE} -C "bash -s"
fi

mkdir -p "$(dirname "$OUTPUT_PATH")"
echo "Downloading built agent..."
rsync -az \
    -e "${FLY_RSH}" \
    "placeholder:${REMOTE_OUTPUT}" \
    "${OUTPUT_PATH}"
