# BrowserSkill selection:
#   WITH_BROWSERSKILL=1        (default) build extension & daemon from source
#   WITH_BROWSERSKILL=0        skip BrowserSkill build (empty directory, fast dev/hotfix)
#   WITH_BROWSERSKILL=prebuilt copy /opt/weknora/browserskill from BROWSERSKILL_IMAGE
ARG WITH_BROWSERSKILL=1
ARG BROWSERSKILL_IMAGE=debian:12.12-slim

# Runtime base selection:
#   USE_PREBUILT_BASE=0 (default) build runtime-base from debian:12.12-slim + apt + browserskill
#   USE_PREBUILT_BASE=1 start final stage from APP_BASE_IMAGE (skips 1.1GB apt + browserskill)
ARG USE_PREBUILT_BASE=0
ARG APP_BASE_IMAGE=debian:12.12-slim

# Build extension and daemon from the same pinned source on the runtime architecture.
FROM node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e AS browserskill-1
WORKDIR /build
# Use the same optional Debian mirror as the Go builder and runtime stages.
ARG APK_MIRROR_ARG
ARG NPM_REGISTRY
ARG RUSTUP_DIST_SERVER
ARG RUSTUP_UPDATE_ROOT
ARG CARGO_MIRROR_ARG
# Try the configured mirror over HTTP first (e.g. mirrors.aliyun.com serves HTTP
# directly without needing ca-certificates). If the mirror redirects to HTTPS
# (e.g. mirrors.tencent.com), fall back to deb.debian.org only to bootstrap
# ca-certificates before switching to the mirror.
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        cp /etc/apt/sources.list.d/debian.sources /tmp/debian.sources.orig && \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources && \
        { (apt-get update && apt-get install -y --no-install-recommends ca-certificates) || \
          (mv /tmp/debian.sources.orig /etc/apt/sources.list.d/debian.sources && \
           apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
           sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources); } && \
        rm -f /tmp/debian.sources.orig; \
    fi && \
    apt-get update && \
    apt-get install -y --no-install-recommends git python3 ca-certificates curl build-essential cmake pkg-config && \
    rm -rf /var/lib/apt/lists/*
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:$PATH
RUN if [ -n "$RUSTUP_DIST_SERVER" ]; then export RUSTUP_DIST_SERVER="$RUSTUP_DIST_SERVER"; fi && \
    if [ -n "$RUSTUP_UPDATE_ROOT" ]; then export RUSTUP_UPDATE_ROOT="$RUSTUP_UPDATE_ROOT"; fi && \
    if [ -n "$RUSTUP_DIST_SERVER" ]; then \
        curl --proto '=https' --tlsv1.2 -sSf "${RUSTUP_DIST_SERVER%/}/rustup-init.sh" | sh -s -- -y --profile minimal --default-toolchain stable; \
    else \
        curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain stable; \
    fi && \
    if [ -n "$CARGO_MIRROR_ARG" ]; then \
        mkdir -p "$CARGO_HOME" && \
        printf '[source.crates-io]\nreplace-with = "mirror"\n[source.mirror]\nregistry = "%s"\n' "$CARGO_MIRROR_ARG" > "$CARGO_HOME/config.toml"; \
    fi
COPY scripts/build_browserskill.sh scripts/browserskill-release.json ./scripts/
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    --mount=type=cache,target=/root/.local/share/pnpm/store \
    if [ -n "$NPM_REGISTRY" ]; then export npm_config_registry="$NPM_REGISTRY"; fi && \
    bash scripts/build_browserskill.sh /opt/weknora/browserskill "${TARGETOS}/${TARGETARCH}"

FROM debian:12.12-slim AS browserskill-0
RUN mkdir -p /opt/weknora/browserskill

FROM ${BROWSERSKILL_IMAGE} AS browserskill-prebuilt

FROM browserskill-${WITH_BROWSERSKILL} AS browserskill

# Build stage
FROM golang:1.26-bookworm AS builder

WORKDIR /app

# 通过构建参数接收敏感信息
ARG GOPRIVATE_ARG
ARG GOPROXY_ARG
ARG GOSUMDB_ARG=off
ARG APK_MIRROR_ARG

# 设置Go环境变量
ENV GOPRIVATE=${GOPRIVATE_ARG}
ENV GOPROXY=${GOPROXY_ARG}
ENV GOSUMDB=${GOSUMDB_ARG}

# Install dependencies
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
    fi && \
    apt-get update && \
    apt-get install -y git build-essential libsqlite3-dev curl

# Install migrate tool
RUN go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# Copy go mod files. go.mod replace-points anydoc at ./third_party/anydoc-go,
# so that module's go.mod must exist before `go mod download`.
COPY go.mod go.sum ./
COPY third_party/anydoc-go/go.mod third_party/anydoc-go/go.mod
# Mount /go/pkg/mod as a BuildKit cache across all Go steps so modules are never
# re-downloaded and do not bloat image layers (~2.5 GB saved from GC quota).
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd/download cmd/download
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go run cmd/download/duckdb/duckdb.go

# Link the anydoc parser engine (office docs converted in-process, no
# Python docreader). Default on so Hub / compose images ship a working
# engine; pass WITH_ANYDOC=0 to skip the Rust toolchain (~few minutes and
# ~1 GB of build-stage layers).
ARG WITH_ANYDOC=1
ARG RUSTUP_DIST_SERVER
ARG RUSTUP_UPDATE_ROOT
ARG CARGO_MIRROR_ARG
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:$PATH
COPY third_party/anydoc-go third_party/anydoc-go
COPY scripts/build-anydoc-lib.sh scripts/build-anydoc-lib.sh
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    if [ "$WITH_ANYDOC" = "1" ]; then \
        if [ -n "$RUSTUP_DIST_SERVER" ]; then export RUSTUP_DIST_SERVER="$RUSTUP_DIST_SERVER"; fi && \
        if [ -n "$RUSTUP_UPDATE_ROOT" ]; then export RUSTUP_UPDATE_ROOT="$RUSTUP_UPDATE_ROOT"; fi && \
        if [ -n "$RUSTUP_DIST_SERVER" ]; then \
            curl --proto '=https' --tlsv1.2 -sSf "${RUSTUP_DIST_SERVER%/}/rustup-init.sh" \
                | sh -s -- -y --profile minimal --default-toolchain stable; \
        else \
            curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
                | sh -s -- -y --profile minimal --default-toolchain stable; \
        fi && \
        if [ -n "$CARGO_MIRROR_ARG" ]; then \
            mkdir -p "$CARGO_HOME" && \
            printf '[source.crates-io]\nreplace-with = "mirror"\n[source.mirror]\nregistry = "%s"\n' "$CARGO_MIRROR_ARG" > "$CARGO_HOME/config.toml"; \
        fi && \
        ./scripts/build-anydoc-lib.sh; \
    fi

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod

# Only the final Go build depends on application source and version metadata.
ARG VERSION_ARG
ARG COMMIT_ID_ARG
ARG BUILD_TIME_ARG
ENV VERSION=${VERSION_ARG}
ENV COMMIT_ID=${COMMIT_ID_ARG}
ENV BUILD_TIME=${BUILD_TIME_ARG}

# Isolate cached cgo outputs when installed native libraries or AnyDoc change.
# Read the Go version here so host machines without Go still report it correctly.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    export GO_VERSION="$(go version)"; \
    native_cache_key="$( { dpkg-query -W -f='${Package}=${Version}\n'; go env GOOS GOARCH; echo "$GO_VERSION"; \
        if [ "$WITH_ANYDOC" = "1" ]; then sha256sum third_party/anydoc-go/lib/linux_*_gnu/libanydoc_go.a; fi; \
    } | sha256sum | cut -d' ' -f1)"; \
    export GOCACHE="/root/.cache/go-build/$native_cache_key"; \
    mkdir -p "$GOCACHE"; \
    if [ "$WITH_ANYDOC" = "1" ]; then \
        make build-prod GO_BUILD_TAGS=anydoc; \
    else \
        make build-prod; \
    fi
RUN --mount=type=cache,target=/go/pkg/mod \
    cp -r /go/pkg/mod/github.com/yanyiwu/ /app/yanyiwu/

# Runtime base stage (can be built once with `--target runtime-base` and pushed
# to a registry as a shared base image, e.g. harbor.dobest.com/sdk/weknora-app-base:v1).
FROM debian:12.12-slim AS runtime-base-0

WORKDIR /app

ARG APK_MIRROR_ARG
ARG PIP_INDEX_URL

# Create a non-root user first
RUN useradd -m -s /bin/bash appuser

# Install ca-certificates (try configured mirror over HTTP first; fall back to
# deb.debian.org only if the mirror forces HTTPS before a trust store exists).
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        cp /etc/apt/sources.list.d/debian.sources /tmp/debian.sources.orig && \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources && \
        { (apt-get update && apt-get install -y --no-install-recommends ca-certificates) || \
          (mv /tmp/debian.sources.orig /etc/apt/sources.list.d/debian.sources && \
           apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
           sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources); } && \
        rm -f /tmp/debian.sources.orig; \
    else \
        apt-get update && apt-get install -y --no-install-recommends ca-certificates; \
    fi && \
    rm -rf /var/lib/apt/lists/*

# Install runtime dependencies (kept before COPY --from=browserskill/builder so
# rebuilding application or browserskill layers never invalidates this layer).
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        build-essential postgresql-client default-mysql-client tzdata sed curl bash vim wget \
        libsqlite3-0 \
        python3 python3-pip python3-dev libffi-dev libssl-dev \
        nodejs npm \
        gosu \
        ffmpeg qpdf && \
    if [ -n "$PIP_INDEX_URL" ]; then \
        python3 -m pip install --no-cache-dir --break-system-packages -i "$PIP_INDEX_URL" --upgrade pip setuptools wheel; \
    else \
        python3 -m pip install --no-cache-dir --break-system-packages --upgrade pip setuptools wheel; \
    fi && \
    mkdir -p /home/appuser/.local/bin && \
    curl -LsSf https://astral.sh/uv/install.sh | CARGO_HOME=/home/appuser/.cargo UV_INSTALL_DIR=/home/appuser/.local/bin sh && \
    chown -R appuser:appuser /home/appuser && \
    ln -sf /home/appuser/.local/bin/uvx /usr/local/bin/uvx && \
    chmod +x /usr/local/bin/uvx && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Create data directories and set permissions
RUN mkdir -p /data/files && \
    chown -R appuser:appuser /app /data/files

# Pairing derives the gateway URL from the user's page origin by default.
ENV BROWSERSKILL_BINARY=/opt/weknora/browserskill/bsk \
    BROWSERSKILL_EXTENSION_PATH=/opt/weknora/browserskill/browser-skill-weknora-0.4.1.zip
COPY --from=browserskill /opt/weknora/browserskill /opt/weknora/browserskill

FROM runtime-base-0 AS runtime-base

FROM ${APP_BASE_IMAGE} AS runtime-base-1

# Final stage
FROM runtime-base-${USE_PREBUILT_BASE} AS final

WORKDIR /app

# Copy migrate tool from builder stage
COPY --from=builder /go/bin/migrate /usr/local/bin/
COPY --from=builder /app/yanyiwu/ /go/pkg/mod/github.com/yanyiwu/

# Copy the binary from the builder stage
COPY --from=builder /app/config ./config
COPY --from=builder /app/scripts ./scripts
COPY --from=builder /app/migrations ./migrations
COPY --from=builder /app/dataset/samples ./dataset/samples
COPY --from=builder /root/.duckdb /home/appuser/.duckdb
COPY --from=builder /app/WeKnora .


# Copy and make entrypoint script executable
COPY --from=builder /app/scripts/docker-entrypoint.sh ./scripts/docker-entrypoint.sh

# Make scripts executable
RUN chmod +x ./scripts/*.sh

# Expose ports
EXPOSE 8080


ENTRYPOINT ["./scripts/docker-entrypoint.sh"]
CMD ["./WeKnora"]
