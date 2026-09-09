# Teraflock flockd — canonical build interface. The same verbs work in every
# Teraflock repo where they apply: make build / test / run / gen / lint / clean.
#
# `gen` regenerates the management API router+types and the Go client from
# api/openapi.yaml (oapi-codegen, pinned via the go.mod tool directive).
# build and test depend on it, so the spec, the daemon and every Go client
# (tera CLI/TUI, tera mcp) cannot drift.

VERSION ?= $(shell git rev-parse --short HEAD)-dev

.PHONY: build test test-race gen gen-check run vet web smoke clean \
	lint lint-tools lint-revive lint-staticcheck lint-http lint-vuln

build: gen
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o bin/flockd ./cmd/flockd
	go build -ldflags "-X main.version=$(VERSION)" -o bin/tera ./cmd/tera

gen:
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	go tool oapi-codegen -config api/oapi-codegen-client.yaml api/openapi.yaml
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

# Lint tool versions — identical to control-plane's Makefile, which is the
# canonical copy of the whole lint policy (revive.toml included; see
# teraflock/flockd#15). CI caches $(GOBIN) keyed on this file, so bump both
# together. lint-tools only installs what is missing — safe to call every
# time without paying a reinstall — which also means a stale local binary
# is kept: `rm $(GOBIN)/<tool>` to pick up a bump. staticcheck and the
# noctxcheck wrapper need a Go 1.26 toolchain to build; with the default
# GOTOOLCHAIN=auto the go command fetches one, the repo itself stays on
# the go.mod toolchain.
GOBIN               := $(shell go env GOPATH)/bin
REVIVE_VERSION      := v1.15.0
STATICCHECK_VERSION := v0.8.1
GOVULNCHECK_VERSION := v1.8.0
BODYCLOSE_VERSION   := v0.0.0-20260723120731-857993a2939c

lint-tools:
	test -x "$(GOBIN)/revive"      || go install github.com/mgechev/revive@$(REVIVE_VERSION)
	test -x "$(GOBIN)/staticcheck" || go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	test -x "$(GOBIN)/govulncheck" || go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	test -x "$(GOBIN)/bodyclose"   || go install github.com/timakin/bodyclose@$(BODYCLOSE_VERSION)
	test -x "$(GOBIN)/noctxcheck"  || (cd tools/noctxcheck && go build -o "$(GOBIN)/noctxcheck" .)

# The whole lint policy (teraflock/flockd#15, mirrors control-plane#9).
# Every step exits non-zero on a finding; CI runs the lint-* targets as
# separate steps so a failure names the tool that raised it.
lint: lint-revive lint-staticcheck lint-http lint-vuln

lint-revive: lint-tools
	"$(GOBIN)/revive" -config revive.toml -formatter friendly ./...

lint-staticcheck: lint-tools
	"$(GOBIN)/staticcheck" ./...

# Leaked response bodies and context-less HTTP/exec/listen calls. Non-test
# code only (context-less httptest requests are idiomatic in tests), and
# not the oapi-codegen output: its client builds requests with
# http.NewRequest + WithContext and returns responses for the caller to
# close, which both analyzers report and nobody may edit by hand.
lint-http: lint-tools
	"$(GOBIN)/bodyclose"  -test=false $$(go list ./... | grep -v /internal/localapi/gen)
	"$(GOBIN)/noctxcheck" -test=false $$(go list ./... | grep -v /internal/localapi/gen)

# Known CVEs reachable from our code (stdlib + modules). Needs network for
# the vuln DB. Stdlib findings depend on the toolchain: go.mod pins the
# patch release so local runs and CI scan the same one.
lint-vuln: lint-tools
	"$(GOBIN)/govulncheck" ./...

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
