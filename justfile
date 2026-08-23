# Teraflock flockd task runner (SPEC §A3: `just build|test|lint|run`).
# Everything also works with plain go commands — just is convenience.

default: build

# Build both binaries into ./dist/local
build:
    mkdir -p dist/local
    go build -o dist/local/flockd ./cmd/flockd
    go build -o dist/local/flock ./cmd/flock

# Run all tests
test:
    go test ./...

# Race-enabled tests for the concurrency-heavy packages
test-race:
    go test -race ./internal/governor/ ./internal/tunnel/... ./internal/runtime/... ./internal/localapi/

# Lint (installs revive on first use — same tool CI runs)
lint:
    go install github.com/mgechev/revive@v1.15.0
    $(go env GOPATH)/bin/revive -formatter friendly ./...

vet:
    go vet ./...

# Run the daemon standalone with the mock runtime
run:
    go run ./cmd/flockd --standalone --runtime=mock

# End-to-end Phase 0 smoke test
smoke:
    ./scripts/smoke.sh

# Rebuild the embedded web dashboard
web:
    cd web && npm install && npm run build

# Live TUI against a running daemon
dashboard:
    go run ./cmd/flock dashboard

clean:
    rm -rf dist
