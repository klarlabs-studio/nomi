.PHONY: all build dev test clean app-dev app-build migrate sidecar roady-guard reliability-evals

# Build metadata injected into internal/buildinfo via -ldflags. CI overrides
# VERSION with the release tag (e.g. VERSION=v0.2.0 make build); local builds
# fall back to `git describe` so dirty checkouts surface as e.g. v0.1.0-3-gabc-dirty.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/felixgeelhaar/nomi/internal/buildinfo.Version=$(VERSION) \
	-X github.com/felixgeelhaar/nomi/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/felixgeelhaar/nomi/internal/buildinfo.BuildDate=$(BUILD_DATE)

# Default target
all: build

# Development
# Run the Go backend with hot reload (requires air: go install github.com/cosmtrek/air@latest)
dev:
	@echo "Starting nomid development server..."
	@go run -ldflags "$(LDFLAGS)" cmd/nomid/main.go

# Production build (daemon + CLI client). The CLI ships as a separate
# binary so headless users / CI / SSH-driven workflows can install
# just `nomi` without pulling in the daemon and its WASM host.
build: build-daemon build-cli

build-daemon:
	@echo "Building nomid $(VERSION) (commit $(COMMIT))..."
	@go build -ldflags "$(LDFLAGS)" -o bin/nomid ./cmd/nomid

build-cli:
	@echo "Building nomi CLI $(VERSION)..."
	@go build -ldflags "$(LDFLAGS)" -o bin/nomi ./cmd/nomi

# Run tests
test:
	@echo "Running tests..."
	@go test -v -race -coverprofile=coverage.out ./...

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/ dist/ coverage.out

# Database migrations
migrate-up:
	@echo "Running migrations..."
	@go run cmd/migrate/main.go up

migrate-down:
	@echo "Rolling back migrations..."
	@go run cmd/migrate/main.go down

migrate-create:
	@read -p "Migration name: " name; \
	migrate create -ext sql -dir migrations -seq $$name

# Tauri's externalBin convention expects bin/nomid-<host-target-triple>
# inside src-tauri/. Build for the running platform's triple before
# `tauri dev` / `tauri build` so the bundler can copy it into the
# produced .app / .msi / .deb. CI builds for all targets in release.yml;
# this target covers the local dev loop.
HOST_TRIPLE := $(shell rustc -vV 2>/dev/null | sed -n 's/host: //p')
SIDECAR_EXT := $(if $(findstring windows,$(HOST_TRIPLE)),.exe,)

sidecar:
	@if [ -z "$(HOST_TRIPLE)" ]; then \
		echo "rustc not found on PATH; install Rust toolchain to determine the target triple"; \
		exit 1; \
	fi
	@echo "Building nomid sidecar for $(HOST_TRIPLE)..."
	@mkdir -p app/src-tauri/bin
	@go build -trimpath -ldflags "$(LDFLAGS)" \
		-o "app/src-tauri/bin/nomid-$(HOST_TRIPLE)$(SIDECAR_EXT)" \
		./cmd/nomid

# Tauri app
app-dev: sidecar
	@echo "Starting Tauri app..."
	@cd app && npm run tauri dev

app-build: sidecar
	@echo "Building Tauri app (sidecar pre-staged at app/src-tauri/bin/nomid-$(HOST_TRIPLE))..."
	@cd app && npm run tauri build

# Dependencies
deps:
	@echo "Downloading Go dependencies..."
	@go mod download
	@echo "Installing app dependencies..."
	@cd app && npm install

# Lint
lint:
	@echo "Running linter..."
	@golangci-lint run ./...

# Format
fmt:
	@echo "Formatting code..."
	@go fmt ./...
	@cd app && npx prettier --write .

# Rebuild the example WASM plugin used by the wasmhost spike test.
# Requires TinyGo on PATH (`brew install tinygo` on macOS).
wasm-echo:
	@echo "Building examples/wasm-plugin-echo to internal/plugins/wasmhost/testdata/echo.wasm..."
	@command -v tinygo >/dev/null || { echo "TinyGo not found. Install via: brew install tinygo"; exit 1; }
	@tinygo build -o internal/plugins/wasmhost/testdata/echo.wasm -target=wasi -no-debug ./examples/wasm-plugin-echo/
	@ls -la internal/plugins/wasmhost/testdata/echo.wasm

# Rebuild the standard-Go variant of the echo plugin (Go 1.24+ wasip1
# reactor mode). Used by bench_compare_test.go to compare TinyGo vs
# standard Go on binary size + cold-start latency.
wasm-echo-stdgo:
	@echo "Building examples/wasm-plugin-echo-stdgo to internal/plugins/wasmhost/testdata/echo-stdgo.wasm..."
	@GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o internal/plugins/wasmhost/testdata/echo-stdgo.wasm ./examples/wasm-plugin-echo-stdgo/
	@ls -la internal/plugins/wasmhost/testdata/echo-stdgo.wasm

# Run the WASM runtime comparison benchmarks.
wasm-bench:
	@go test -bench=. -benchmem -run=^$$ -count=3 ./internal/plugins/wasmhost/

# Validate Roady state/plan task-ID governance.
roady-guard:
	@python3 scripts/roady_state_guard.py

# Run reliability eval taxonomy tests.
reliability-evals:
	@go test ./internal/runtime/evals/...

# Run the planner golden corpus + adversarial fixtures against the
# fake LLM. Threshold default is 0.80; tighten via NOMI_GOLDEN_THRESHOLD.
eval-live:
	@NOMI_GOLDEN_THRESHOLD=$${NOMI_GOLDEN_THRESHOLD:-0.80} go test -v -count=1 -run "TestPlannerGoldenSet|TestPlannerAdversarialResilience" ./internal/runtime/evals/

# Run the planner golden corpus against every live LLM provider whose
# envs are configured and emit a per-provider pass-rate report. Skips
# silently when nothing is configured so a developer can run only the
# providers they have credentials for.
#
# Configure providers via env (any subset):
#   NOMI_EVAL_LIVE_OLLAMA_MODEL=qwen2.5:14b        # + optional NOMI_EVAL_LIVE_OLLAMA_URL
#   OPENAI_API_KEY=...  NOMI_EVAL_LIVE_OPENAI_MODEL=gpt-4o-mini
#   ANTHROPIC_API_KEY=...  NOMI_EVAL_LIVE_ANTHROPIC_MODEL=claude-sonnet-4-6
#
# Per-provider threshold override (default falls back to NOMI_GOLDEN_THRESHOLD):
#   NOMI_GOLDEN_THRESHOLD_OLLAMA=0.6
#   NOMI_GOLDEN_THRESHOLD_OPENAI=0.85
#   NOMI_GOLDEN_THRESHOLD_ANTHROPIC=0.85
eval-live-providers:
	@go test -v -count=1 -timeout 30m -run TestPlannerGoldenSet_Live ./internal/runtime/evals/
