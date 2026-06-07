# Headless nomid daemon — for users running Nomi on a server / homelab
# without the Tauri desktop shell. The image ships ONLY the Go runtime
# binary, embedded migrations, and the bundled WASM plugins. Pair with
# Ollama (or any OpenAI-compatible LLM endpoint) running on the host or
# as a sibling container.
#
# Quick start:
#   docker build -t nomi/nomid .
#   docker run --rm \
#     -p 8080:8080 \
#     -v nomi-data:/data \
#     nomi/nomid
#
# The on-disk SQLite database, auth token, and api.endpoint marker all
# land under /data. Mount that as a named volume (or a host path) so
# state survives container restarts.

# ---- builder ---------------------------------------------------------------
FROM golang:1.26-alpine AS builder

# CGO is off — modernc.org/sqlite is pure Go, no libc bindings needed.
# git is required for `go install` to resolve module proxies in some
# corp networks; alpine's base image lacks it.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy module manifests first so the layer cache survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Bake the build metadata the daemon shows on `nomid --version` and
# /version. Tag-based builds via the release pipeline overwrite this
# layer with the actual values; local docker builds pin "docker" so the
# binary is identifiable.
ARG NOMI_VERSION=docker
ARG NOMI_COMMIT=local
ARG NOMI_BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X go.klarlabs.de/nomi/internal/buildinfo.Version=${NOMI_VERSION} \
      -X go.klarlabs.de/nomi/internal/buildinfo.Commit=${NOMI_COMMIT} \
      -X go.klarlabs.de/nomi/internal/buildinfo.BuildDate=${NOMI_BUILD_DATE}" \
    -o /out/nomid \
    ./cmd/nomid

# ---- runtime --------------------------------------------------------------
# Distroless static is ~2 MB, ships only ca-certificates + tzdata, and
# runs as nonroot by default. No shell → smaller attack surface.
FROM gcr.io/distroless/static-debian12:nonroot

# /data carries the entire stateful surface: nomi.db, auth.token,
# api.endpoint, and the secrets vault file. Documented as a volume so
# the user is nudged into mounting it.
VOLUME ["/data"]
ENV NOMI_DATA_DIR=/data \
    NOMI_API_PORT=8080

EXPOSE 8080

COPY --from=builder /out/nomid /usr/local/bin/nomid

# Bind to all interfaces inside the container — the host port-mapping
# decides what's reachable from outside. The auth token still gates
# every request, so accidentally exposing the port on the host is
# annoying (token in /data) but not catastrophic.
ENV NOMI_BIND=0.0.0.0
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/nomid"]
