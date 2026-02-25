# Weft - Development Commands

# Default: show available commands
default:
    @just --list

# Build the binary
build:
    go build -o weft .

# Install to $GOPATH/bin
install:
    go install .

# Run tests
test:
    go test ./...

# Run tests with verbose output
test-verbose:
    go test -v ./...

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

# Format code
format:
    go fmt ./...

# Run linter
lint:
    go vet ./...

# Check: format, lint, test
check: format lint test

# Clean build artifacts
clean:
    rm -f weft

# Model-check the PlusCal reconciliation spec
tla-check:
    internal/scripts/run_tla_checks.sh

# Model-check the PlusCal spec with Apalache
tla-apalache:
    JAVA_BIN=${JAVA_BIN:-java} TLA_JAR=${TLA_JAR:-$HOME/lib/tla2tools.jar} internal/scripts/run_tla_apalache.sh

