# Weft - Development Commands

# Default: show available commands
default:
    @just --list

# Build the binary and agent binaries in parallel
build:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Building weft binary and agent binaries in parallel..."
    pids=()
    just build-agents &
    pids+=($!)
    go build -o weft . &
    pids+=($!)
    for pid in "${pids[@]}"; do wait "$pid" || exit 1; done
    echo "Build complete."

# Install to $GOPATH/bin
install:
    go install .

# Run tests (skips slow build tests; use test-all for full suite)
test:
    go test -short ./...

# Run all tests including slow build tests
test-all:
    go test ./...

# Run tests with verbose output
test-verbose:
    go test -short -v ./...

# Run integration tests (requires .env with SSH_TEST_HOST)
# Note: SLURM tests skipped due to SLURM scheduler issues on test server
test-integration:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -f .env ]; then
        export $(grep -v '^#' .env | xargs)
    fi
    if [ -z "${SSH_TEST_HOST:-}" ]; then
        echo "SSH_TEST_HOST not set. Create .env file with SSH_TEST_HOST=user@host"
        exit 1
    fi
    go test -v ./internal/ops/... -run "Integration" -skip "TestSlurmIntegration" -timeout 300s

# Run Go runner integration tests (requires .env with SSH_TEST_HOST and deployed agent)
test-runner-integration:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -f .env ]; then
        export $(grep -v '^#' .env | xargs)
    fi
    if [ -z "${SSH_TEST_HOST:-}" ]; then
        echo "SSH_TEST_HOST not set. Create .env file with SSH_TEST_HOST=user@host"
        exit 1
    fi
    echo "Deploying agent binary to ${SSH_TEST_HOST}..."
    go run . sync "${SSH_TEST_HOST}" 2>/dev/null || true
    echo "Running Go runner integration tests..."
    go test -v ./internal/runner/... -run "Integration" -timeout 120s

# Format code
format:
    go fmt ./...

# Run linter
lint:
    go vet ./...

# Check: format, lint, test
check: format lint test

# Build agent binaries into internal/agentdeploy/binaries/
build-agents:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p internal/agentdeploy/binaries
    VERSION=$(jj log --no-graph -r 'ancestors(@-, 200)' -T 'commit_id.short(12)' --limit 1 cmd/agent/ internal/ 2>/dev/null || echo "dev")
    LDFLAGS="-X main.version=${VERSION}"
    echo "Building agent binaries in parallel (version: ${VERSION})..."
    pids=()

    # linux/amd64: use Fly builder if configured, otherwise skip
    if [ -n "${WEFT_FLY_BUILDER_APP:-}" ] && [ -n "${WEFT_FLY_BUILDER_MACHINE:-}" ]; then
        ./scripts/build-agent-on-fly.sh "${VERSION}" internal/agentdeploy/binaries/weft-agent-linux-amd64 &
        pids+=($!)
    else
        echo "info: WEFT_FLY_BUILDER_APP/MACHINE not set; skipping linux/amd64 agent build"
        echo "      set these env vars to enable Fly-based cross-compilation"
    fi

    # darwin/arm64: build on WEFT_MACOS_BUILDER_HOST via SSH if set, otherwise build locally
    if [ -n "${WEFT_MACOS_BUILDER_HOST:-}" ]; then
        (
            REMOTE_DIR="${WEFT_MACOS_BUILDER_DIR:-~/.cache/weft/agent-build}"
            rsync -az --delete \
                --exclude='.git/' --exclude='.jj/' --exclude='.claude/' \
                --exclude='.cache/' --exclude='.gocache/' --exclude='.gomodcache/' \
                --exclude='.bench-*-gocache/' --exclude='.bench-*-gomodcache/' \
                --exclude='testdata/' --exclude='dist/' \
                '--exclude=internal/agentdeploy/binaries/weft-agent-*' \
                '--exclude=internal/agentdeploy/binaries/VERSION' \
                '--exclude=weft' '--exclude=placement.test' \
                ./ "${WEFT_MACOS_BUILDER_HOST}:${REMOTE_DIR}/"
            ssh "${WEFT_MACOS_BUILDER_HOST}" \
                "cd ${REMOTE_DIR} && GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o internal/agentdeploy/binaries/weft-agent-darwin-arm64 ./cmd/agent"
            rsync -az \
                "${WEFT_MACOS_BUILDER_HOST}:${REMOTE_DIR}/internal/agentdeploy/binaries/weft-agent-darwin-arm64" \
                internal/agentdeploy/binaries/weft-agent-darwin-arm64
        ) &
        pids+=($!)
    else
        GOOS=darwin GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o internal/agentdeploy/binaries/weft-agent-darwin-arm64 ./cmd/agent &
        pids+=($!)
    fi

    for pid in "${pids[@]}"; do wait "$pid" || exit 1; done
    echo "${VERSION}" > internal/agentdeploy/binaries/VERSION

# Build agent binary for a target (default: current platform)
build-agent target="local":
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    VERSION=$(jj log --no-graph -r 'ancestors(@-, 200)' -T 'commit_id.short(12)' --limit 1 cmd/agent/ internal/ 2>/dev/null || echo "dev")
    LDFLAGS="-X main.version=${VERSION}"
    case "{{ target }}" in
        local)
            go build -ldflags "${LDFLAGS}" -o dist/weft-agent ./cmd/agent
            echo "Built dist/weft-agent (version: ${VERSION})"
            ;;
        linux-amd64)
            if [ -n "${WEFT_FLY_BUILDER_APP:-}" ] && [ -n "${WEFT_FLY_BUILDER_MACHINE:-}" ]; then
                ./scripts/build-agent-on-fly.sh "${VERSION}" dist/weft-agent-linux-amd64
            else
                echo "info: WEFT_FLY_BUILDER_APP/MACHINE not set; attempting local cross-compile (may fail without gcc cross-toolchain)"
                GOOS=linux GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/weft-agent-linux-amd64 ./cmd/agent
            fi
            echo "Built dist/weft-agent-linux-amd64 (version: ${VERSION})"
            ;;
        darwin-arm64)
            GOOS=darwin GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o dist/weft-agent-darwin-arm64 ./cmd/agent
            echo "Built dist/weft-agent-darwin-arm64 (version: ${VERSION})"
            ;;
        all)
            GOOS=linux GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/weft-agent-linux-amd64 ./cmd/agent
            GOOS=darwin GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o dist/weft-agent-darwin-arm64 ./cmd/agent
            echo "Built all agent binaries (version: ${VERSION})"
            ;;
        *)
            echo "Unknown target: {{ target }}. Use: local, linux-amd64, darwin-arm64, all"
            exit 1
            ;;
    esac

# Build on a remote host and sync artifacts back — faster than local cross-compile
# Set WEFT_BUILD_HOST in .env or environment (e.g., WEFT_BUILD_HOST=studio)
remote-build:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -f .env ]; then
        export $(grep -v '^#' .env | xargs)
    fi
    HOST="${WEFT_BUILD_HOST:-}"
    if [ -z "$HOST" ]; then
        echo "WEFT_BUILD_HOST not set. Add WEFT_BUILD_HOST=<host> to .env or environment."
        exit 1
    fi
    REMOTE_DIR="~/code/utils/weft"

    echo "==> Syncing sources to ${HOST}..."
    rsync -az --delete \
        --exclude='.jj/' --exclude='.git/' --exclude='.claude/' \
        --exclude='.gocache/' --exclude='.gomodcache/' --exclude='.cache/' \
        --exclude='.bench-*-gocache/' --exclude='.bench-*-gomodcache/' \
        --exclude='weft' --exclude='dist/' \
        --exclude='internal/agentdeploy/binaries/weft-agent-*' \
        --exclude='internal/agentdeploy/binaries/VERSION' \
        ./ "${HOST}:${REMOTE_DIR}/"

    echo "==> Building on ${HOST}..."
    ssh "${HOST}" "cd ${REMOTE_DIR} && just build"

    echo "==> Syncing build artifacts back..."
    rsync -az "${HOST}:${REMOTE_DIR}/weft" ./weft
    rsync -az "${HOST}:${REMOTE_DIR}/internal/agentdeploy/binaries/" ./internal/agentdeploy/binaries/

    echo "==> Done. Local binary and agent binaries updated."

# Deploy agent binary to a remote host (default: WEFT_DEPLOY_HOST from .env)
# Builds for the target platform, deploys via scp + atomic rename, and kills the runner session.
deploy-agent host="":
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -f .env ]; then
        export $(grep -v '^#' .env | xargs)
    fi
    HOST="{{ host }}"
    if [ -z "$HOST" ]; then
        HOST="${WEFT_DEPLOY_HOST:-}"
    fi
    if [ -z "$HOST" ]; then
        echo "Usage: just deploy-agent <host>"
        echo "Or set WEFT_DEPLOY_HOST in .env"
        exit 1
    fi

    # Determine target platform
    TARGET_OS=$(ssh "$HOST" uname -s | tr '[:upper:]' '[:lower:]')
    TARGET_ARCH=$(ssh "$HOST" uname -m)
    case "$TARGET_ARCH" in
        x86_64) TARGET_ARCH="amd64" ;;
        aarch64|arm64) TARGET_ARCH="arm64" ;;
    esac

    echo "==> Building agent for ${TARGET_OS}-${TARGET_ARCH}..."
    mkdir -p dist
    VERSION=$(jj log --no-graph -r 'ancestors(@-, 200)' -T 'commit_id.short(12)' --limit 1 cmd/agent/ internal/ 2>/dev/null || echo "dev")
    LDFLAGS="-X main.version=${VERSION}"
    GOOS="${TARGET_OS}" GOARCH="${TARGET_ARCH}" go build -ldflags "${LDFLAGS}" -o "dist/weft-agent-${TARGET_OS}-${TARGET_ARCH}" ./cmd/agent

    REMOTE_BIN="~/.cache/weft/bin/weft-agent"
    echo "==> Deploying to ${HOST}..."
    ssh "$HOST" "mkdir -p ~/.cache/weft/bin"
    scp "dist/weft-agent-${TARGET_OS}-${TARGET_ARCH}" "${HOST}:${REMOTE_BIN}.tmp"
    ssh "$HOST" "chmod +x ${REMOTE_BIN}.tmp && mv ${REMOTE_BIN}.tmp ${REMOTE_BIN}"

    echo "==> Killing runner session on ${HOST} (will restart with new binary)..."
    ssh "$HOST" "tmux kill-session -t weft-runner 2>/dev/null" || true

    echo "==> Done. Agent ${VERSION} deployed to ${HOST}."

# Deploy coordinator to studio: sync sources, rebuild, restart the launchd service
deploy-coordinator host="studio":
    #!/usr/bin/env bash
    set -euo pipefail
    HOST="{{ host }}"
    REMOTE_DIR="~/code/utils/weft"

    echo "==> Syncing sources to ${HOST}..."
    rsync -az --delete \
        --exclude='.jj/' --exclude='.git/' --exclude='.claude/' \
        --exclude='.gocache/' --exclude='.gomodcache/' --exclude='.cache/' \
        --exclude='.bench-*-gocache/' --exclude='.bench-*-gomodcache/' \
        --exclude='weft' --exclude='dist/' \
        --exclude='internal/agentdeploy/binaries/weft-agent-*' \
        --exclude='internal/agentdeploy/binaries/VERSION' \
        ./ "${HOST}:${REMOTE_DIR}/"

    echo "==> Building on ${HOST}..."
    ssh "${HOST}" "cd ${REMOTE_DIR} && go build -o ~/.cache/weft/bin/weft ."

    echo "==> Restarting coordinator on ${HOST}..."
    # KeepAlive=true in launchd plist means it auto-restarts after stop
    ssh "${HOST}" "~/.cache/weft/bin/weft coordinator stop 2>/dev/null || true"
    sleep 2
    ssh "${HOST}" "~/.cache/weft/bin/weft coordinator status"

    echo "==> Done. Coordinator deployed and restarted on ${HOST}."

# Clean build artifacts
clean:
    rm -f weft
    rm -rf dist
    rm -f internal/agentdeploy/binaries/weft-agent-* internal/agentdeploy/binaries/VERSION
