# HiveGrid hived task runner (SPEC §A3: `just build|test|lint|run`).
# Everything also works with plain go commands — just is convenience.

default: build

# Build both binaries into ./dist/local
build:
    mkdir -p dist/local
    go build -o dist/local/hived ./cmd/hived
    go build -o dist/local/hive ./cmd/hive

# Run all tests
test:
    go test ./...

# Race-enabled tests for the concurrency-heavy packages
test-race:
    go test -race ./internal/governor/ ./internal/tunnel/... ./internal/runtime/... ./internal/localapi/

# Lint (requires golangci-lint installed)
lint:
    golangci-lint run

vet:
    go vet ./...

# Run the daemon standalone with the mock runtime
run:
    go run ./cmd/hived --standalone --runtime=mock

# End-to-end Phase 0 smoke test
smoke:
    ./scripts/smoke.sh

# Rebuild the embedded web dashboard
web:
    cd web && npm install && npm run build

# Live TUI against a running daemon
dashboard:
    go run ./cmd/hive dashboard

clean:
    rm -rf dist
