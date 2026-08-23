# Weft - Development Commands

# Default: show available commands
default:
    @just --list

# Build the binary and agent binaries in parallel
build:
    #!/usr/bin/env bash
    set -euo pipefail
    prewarm_pid=""
    start_agent_prewarm() {
        local label="$1"
        if command -v weft >/dev/null 2>&1; then
            PREWARM_LOG="${HOME}/.cache/weft/agent-prewarm.log"
            mkdir -p "$(dirname "${PREWARM_LOG}")"
            echo "Starting background agent prewarm via installed weft (log: ${PREWARM_LOG})..."
            (
                printf '[%s] prewarm start (%s)\n' "$(date -u +%FT%TZ)" "${label}"
                if weft build-agents --targets linux-amd64; then
                    printf '[%s] prewarm ok (%s)\n' "$(date -u +%FT%TZ)" "${label}"
                else
                    status=$?
                    printf '[%s] prewarm failed (%s, exit=%s)\n' "$(date -u +%FT%TZ)" "${label}" "${status}"
                fi
            ) >>"${PREWARM_LOG}" 2>&1 &
            prewarm_pid=$!
        else
            echo "info: installed weft not found; skipping agent prewarm"
        fi
    }
    wait_agent_prewarm() {
        if [ -n "${prewarm_pid}" ]; then
            echo "Waiting for background agent prewarm..."
            wait "${prewarm_pid}" || true
        fi
    }
    cleanup() {
        status=$?
        wait_agent_prewarm
        exit "${status}"
    }
    trap cleanup EXIT
    echo "Scheduling predictor schema check (non-blocking)..."
    (go run . retrain --if-schema-changed >/dev/null 2>&1 || true) &
    echo "Building weft binary..."
    start_agent_prewarm "build"
    go build -o weft .
    wait_agent_prewarm
    trap - EXIT
    echo "Build complete."

# Install to $GOPATH/bin (also builds agents so they are ready to deploy)
install:
    #!/usr/bin/env bash
    set -euo pipefail
    prewarm_pid=""
    start_agent_prewarm() {
        local label="$1"
        if command -v weft >/dev/null 2>&1; then
            PREWARM_LOG="${HOME}/.cache/weft/agent-prewarm.log"
            mkdir -p "$(dirname "${PREWARM_LOG}")"
            # Starts before go install to overlap work with install. The
            # recipe waits for it before exit so chained commands see a quiet
            # build cache and no old weft child process.
            echo "Starting background agent prewarm via installed weft (log: ${PREWARM_LOG})..."
            (
                printf '[%s] prewarm start (%s)\n' "$(date -u +%FT%TZ)" "${label}"
                if weft build-agents --targets linux-amd64; then
                    printf '[%s] prewarm ok (%s)\n' "$(date -u +%FT%TZ)" "${label}"
                else
                    status=$?
                    printf '[%s] prewarm failed (%s, exit=%s)\n' "$(date -u +%FT%TZ)" "${label}" "${status}"
                fi
            ) >>"${PREWARM_LOG}" 2>&1 &
            prewarm_pid=$!
        else
            echo "info: installed weft not found; skipping agent prewarm"
        fi
    }
    wait_agent_prewarm() {
        if [ -n "${prewarm_pid}" ]; then
            echo "Waiting for background agent prewarm..."
            wait "${prewarm_pid}" || true
        fi
    }
    cleanup() {
        status=$?
        wait_agent_prewarm
        exit "${status}"
    }
    trap cleanup EXIT
    echo "Scheduling predictor schema check (non-blocking)..."
    (go run . retrain --if-schema-changed >/dev/null 2>&1 || true) &
    echo "Building local ./weft..."
    go build -o weft .
    echo "Installing weft..."
    start_agent_prewarm "install"
    go install .
    if [ -f "${HOME}/Library/LaunchAgents/com.osteele.weft.daemon.plist" ]; then
        echo "Updating daemon service and transitioning to installed binary..."
        weft daemon install
    fi
    WEFT_DOCS_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/weft/docs"
    mkdir -p "${WEFT_DOCS_DIR}"
    rsync -a --delete docs/ "${WEFT_DOCS_DIR}/"
    cp CLAUDE.md "${WEFT_DOCS_DIR}/CLAUDE.md" 2>/dev/null || true
    cp AGENTS.md "${WEFT_DOCS_DIR}/AGENTS.md" 2>/dev/null || true
    wait_agent_prewarm
    trap - EXIT
    echo "Docs installed to ${WEFT_DOCS_DIR}"
    echo "Install complete."

# Run the current sources and refresh ./weft so local probes don't leave a stale binary behind
run *args:
    #!/usr/bin/env bash
    set -euo pipefail
    go build -o weft .
    go run . {{ args }}

# Run tests (skips slow build tests; use test-all for full suite)
test:
    cgo-test -short ./...

# Run all tests including slow build tests
test-all:
    cgo-test ./...

# Run tests with verbose output
test-verbose:
    cgo-test -short -v ./...

# Run race-detector coverage for daemon/local mutation boundaries and shared DB writers.
# This is intentionally separate from the ordinary test/check loop.
test-race:
	#!/usr/bin/env bash
	set -uo pipefail
	just test-race-db &
	db_pid=$!
	status=0
	if ! cgo-test -race ./internal/daemonapi ./internal/localmutate; then
		status=1
	fi
	if ! wait "${db_pid}"; then
		status=1
	fi
	if ! just test-race-orchestration; then
		status=1
	fi
	exit "${status}"

# Run the large DB race suite in isolated processes. Package globals such as
# dbPath and startupRepairFn make in-process t.Parallel unsafe, while separate
# processes retain isolation and keep each shard below Go's package timeout.
test-race-db:
	#!/usr/bin/env bash
	set -uo pipefail
	pids=()
	for shard in '^Test[A-F]' '^Test[G-N]' '^Test[O-R]' '^Test([^A-R]|$)'; do
		cgo-test -race ./internal/db -run "${shard}" -count=1 &
		pids+=("$!")
	done
	status=0
	for pid in "${pids[@]}"; do
		if ! wait "${pid}"; then
			status=1
		fi
	done
	exit "${status}"

# Run orchestration race tests in isolated processes. These tests also mutate
# package-level lifecycle hooks, so process shards preserve their isolation.
test-race-orchestration:
	#!/usr/bin/env bash
	set -uo pipefail
	pids=()
	for shard in '^Test[A-F]' '^Test[G-Q]' '^TestR' '^Test([^A-R]|$)'; do
		cgo-test -race ./internal/orchestration -run "${shard}" -count=1 &
		pids+=("$!")
	done
	status=0
	for pid in "${pids[@]}"; do
		if ! wait "${pid}"; then
			status=1
		fi
	done
	exit "${status}"

# Regenerate internal/db/testdata/schema.txt, the table-schema golden the
# schema-guard test compares against. Run after a goose migration intentionally
# changes the schema.
regen-schema-golden:
    WEFT_UPDATE_SCHEMA_GOLDEN=1 cgo-test ./internal/db -run TestSchemaMatchesGolden -count=1 -v

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
format *args:
	#!/usr/bin/env bash
	set -euo pipefail
	if (( $# == 0 )); then
		go fmt ./...
		exit
	fi
	go_files=()
	package_args=()
	for arg in "$@"; do
		if [[ "${arg}" == *.go && -f "${arg}" ]]; then
			go_files+=("${arg}")
		else
			package_args+=("${arg}")
		fi
	done
	if (( ${#go_files[@]} > 0 )); then
		gofmt -w "${go_files[@]}"
	fi
	if (( ${#package_args[@]} > 0 )); then
		go fmt "${package_args[@]}"
	fi

# Run linter
lint:
    go vet ./...

# Validate executable behavioral specifications
check-specs:
    bash scripts/check-allium-specs.sh

# Check: format, lint, specs, test
check: format lint check-specs test

# Build and push the cloud bootstrap base image (rclone+uv+apt deps pre-baked)
# to ghcr.io. Requires Docker daemon running and `docker login ghcr.io` for
# the push. Tag defaults to the current jj commit short hash plus :latest.
build-cloud-base-image tag="latest":
    #!/usr/bin/env bash
    set -euo pipefail
    if ! docker version --format '{{{{.Server.Version}}}}' >/dev/null 2>&1; then
        echo "Docker daemon not reachable. Start Docker Desktop and retry." >&2
        exit 1
    fi
    IMAGE="ghcr.io/osteele/weft-cloud-base:{{ tag }}"
    echo "==> Building $IMAGE for linux/amd64..."
    docker buildx build --platform linux/amd64 --push \
        -t "$IMAGE" \
        -f deploy/cloud-base.Dockerfile .
    echo "==> Pushed $IMAGE"
    echo
    echo "Set [vastai] default_image in ~/.config/weft/config.toml to use it:"
    echo "    default_image = \"$IMAGE\""

# Build agent binaries into internal/agentdeploy/binaries/
build-agents:
    #!/usr/bin/env bash
    set -euo pipefail
    if command -v weft >/dev/null 2>&1; then
        if weft build-agents --targets linux-amd64; then
            exit 0
        fi
    fi
    go run . build-agents --targets linux-amd64

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
    REMOTE_DIR="~/code/research-tools/weft"

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
# Delegates to the CLI so host ssh_user/identity config and queue-runner
# lifecycle handling stay in one place.
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
    go run . queue update "${HOST}"

# Clean build artifacts
clean:
    rm -f weft
    rm -rf dist
    rm -f internal/agentdeploy/binaries/weft-agent-* internal/agentdeploy/binaries/VERSION
