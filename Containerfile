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
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GOCACHE=/tmp/gocache

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

# Config (config.yaml), git clones, build contexts and caches live here.
VOLUME /data
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/mcp-machine"]
CMD ["serve","-c","/data/config.yaml"]
