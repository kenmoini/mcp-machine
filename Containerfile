# MCP Machine image. Build with:  make image   (or: podman build -f Containerfile .)

# --- build stage -------------------------------------------------------------
FROM registry.access.redhat.com/ubi9/go-toolset:1.26 AS build

# Named MCPM_* because the go-toolset base image sets ENV VERSION (the Go
# version), which would override a plain ARG VERSION.
ARG MCPM_VERSION=dev
ARG MCPM_COMMIT=none
ARG MCPM_DATE=unknown

# The go-toolset image runs as a non-root user with HOME=/opt/app-root/src, so
# keep every Go cache and module write under paths that user can write.
# GOTOOLCHAIN=auto lets the go command fetch the `toolchain` pinned in go.mod
# (newer than the base image's Go, for patched stdlib); it needs network.
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GOCACHE=/tmp/gocache \
    GOTOOLCHAIN=auto

WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -buildvcs=false \
      -tags remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper \
      -ldflags "-s -w \
        -X github.com/kenmoini/mcp-machine/internal/buildinfo.Version=${MCPM_VERSION} \
        -X github.com/kenmoini/mcp-machine/internal/buildinfo.Commit=${MCPM_COMMIT} \
        -X github.com/kenmoini/mcp-machine/internal/buildinfo.Date=${MCPM_DATE}" \
      -o /tmp/mcp-machine ./cmd/mcp-machine

# --- runtime stage -----------------------------------------------------------
FROM registry.access.redhat.com/ubi9/ubi-minimal:9.8

LABEL org.opencontainers.image.source="https://github.com/kenmoini/mcp-machine" \
      org.opencontainers.image.description="Self-hosted proxy and manager for MCP servers running as Podman containers" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /tmp/mcp-machine /usr/local/bin/mcp-machine

# The admin configuration root: config.yaml plus config.d/*.yaml. Bind-mount a
# host directory read-only over /etc/mcp-machine; the app never writes there.
# The mount is optional: without it the app falls back to the deprecated
# config.yaml directly in /data, with a deprecation warning, or, if that is
# absent too, starts with defaults and a "no configuration found" warning.
RUN mkdir -p /etc/mcp-machine/config.d

# UI-owned state: the config.d/ drop-ins the web UI writes, git clones, build
# contexts and caches live here.
VOLUME /data
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/mcp-machine"]
CMD ["serve","-c","/etc/mcp-machine"]
