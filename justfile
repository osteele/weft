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
