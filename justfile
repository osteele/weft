# Remote Jobs - Development Commands

# Default: show available commands
default:
    @just --list

# Build the binary
build:
    go build -o remote-jobs .

# Install to $GOPATH/bin
install:
    go install .

# Run tests
test:
    go test ./...

# Run tests with verbose output
test-verbose:
    go test -v ./...

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
    rm -f remote-jobs

# Model-check the PlusCal reconciliation spec
tla-check:
    internal/scripts/run_tla_checks.sh

# Model-check the PlusCal spec with Apalache
tla-apalache:
    JAVA_BIN=${JAVA_BIN:-java} TLA_JAR=${TLA_JAR:-$HOME/lib/tla2tools.jar} internal/scripts/run_tla_apalache.sh

