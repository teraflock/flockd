# Teraflock flockd — canonical build interface. The same verbs work in every
# Teraflock repo where they apply: make build / test / run / gen / lint / clean.
#
# `gen` regenerates the management API router+types from api/openapi.yaml
# (oapi-codegen, pinned via the go.mod tool directive). build and test depend
# on it, so the spec and the implementation cannot drift.

VERSION ?= $(shell git rev-parse --short HEAD)-dev

.PHONY: build test test-race gen gen-check run lint vet web smoke clean

build: gen
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o bin/flockd ./cmd/flockd
	go build -ldflags "-X main.version=$(VERSION)" -o bin/tera ./cmd/tera

gen:
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	@gofmt -w internal/localapi/gen/

# CI guard: fails when committed generated code doesn't match the spec.
gen-check: gen
	@git diff --exit-code internal/localapi/gen/ || \
	  (echo "ERROR: generated code out of date — run 'make gen' and commit" && exit 1)

test: gen
	go test ./...

test-race:
	go test -race ./internal/governor/ ./internal/tunnel/... ./internal/runtime/... ./internal/localapi/

# Run the daemon standalone with the mock runtime (no control plane needed).
run: gen
	go run ./cmd/flockd --standalone --runtime=mock

lint:
	test -x "$$(go env GOPATH)/bin/revive" || go install github.com/mgechev/revive@v1.15.0
	"$$(go env GOPATH)/bin/revive" -formatter friendly ./...

vet:
	go vet ./...

# Rebuild the embedded web dashboard (committed dist, embedded via go:embed).
web: web-gen
	cd web && npm install && npm run build

# Regenerate the dashboard's API types from the spec.
web-gen:
	cd web && npx openapi-typescript ../api/openapi.yaml -o src/api.gen.ts

smoke:
	./scripts/smoke.sh

clean:
	rm -rf bin dist
	go clean ./...
