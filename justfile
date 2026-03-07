# Weft - Development Commands

# Default: show available commands
default:
    @just --list

# Build the binary (builds agent binaries for embedding first)
build: build-agents
    go build -o weft .

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

# Build agent binaries for embedding into the weft CLI
build-agents:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p internal/agentdeploy/binaries
    VERSION=$(jj log --no-graph -r 'ancestors(@-, 200)' -T 'commit_id.short(12) ++ "\n"' --limit 1 cmd/agent/ internal/ 2>/dev/null || echo "dev")
    LDFLAGS="-X main.version=${VERSION}"
    GOOS=linux GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o internal/agentdeploy/binaries/weft-agent-linux-amd64 ./cmd/agent
    GOOS=linux GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o internal/agentdeploy/binaries/weft-agent-linux-arm64 ./cmd/agent
    GOOS=darwin GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o internal/agentdeploy/binaries/weft-agent-darwin-arm64 ./cmd/agent
    echo "Built agent binaries for embedding (version: ${VERSION})"

# Build agent binary for a target (default: current platform)
build-agent target="local":
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    VERSION=$(jj log --no-graph -r 'ancestors(@, 50)' -T 'commit_id.short(12) ++ "\n"' --limit 1 cmd/agent/ internal/ 2>/dev/null || echo "dev")
    LDFLAGS="-X main.version=${VERSION}"
    case "{{ target }}" in
        local)
            go build -ldflags "${LDFLAGS}" -o dist/weft-agent ./cmd/agent
            echo "Built dist/weft-agent (version: ${VERSION})"
            ;;
        linux-amd64)
            GOOS=linux GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/weft-agent-linux-amd64 ./cmd/agent
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

# Clean build artifacts
clean:
    rm -f weft
    rm -rf dist
    rm -f internal/agentdeploy/binaries/weft-agent-*

