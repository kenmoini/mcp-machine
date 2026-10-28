GOTAGS ?= remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper
ENGINE ?= podman
IMAGE  ?= ghcr.io/kenmoini/mcp-machine
TAG    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Where `make quadlet-install` puts the units. Rootful by default; for rootless:
#   make quadlet-install QUADLET_DIR=$HOME/.config/containers/systemd SYSTEMCTL="systemctl --user"
QUADLET_DIR ?= /etc/containers/systemd
SYSTEMCTL   ?= systemctl

BUILDINFO = github.com/kenmoini/mcp-machine/internal/buildinfo
LDFLAGS = -s -w \
	-X $(BUILDINFO).Version=$(TAG) \
	-X $(BUILDINFO).Commit=$(COMMIT) \
	-X $(BUILDINFO).Date=$(DATE)

HTMX_URL    = https://unpkg.com/htmx.org@2.0.10/dist/htmx.min.js
HTMX_FILE   = internal/web/static/htmx.min.js
HTMX_SHA256 = 71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de

# Tool versions live here only; CI installs them through `make tools`.
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.9.0
GOBIN_DIR := $(or $(shell go env GOBIN),$(firstword $(subst :, ,$(shell go env GOPATH)))/bin)
export PATH := $(GOBIN_DIR):$(PATH)

# The go.mod toolchain line (go1.27.3) is honoured only when GOTOOLCHAIN is
# "auto". Some Go installations default it to "local" (pinning whatever `go`
# is on PATH, including unpatched toolchains), so set it to "auto" unless the
# caller provides a value. Opt out with: make GOTOOLCHAIN=local <target>.
ifeq ($(GOTOOLCHAIN),)
export GOTOOLCHAIN := auto
endif

.PHONY: build test test-race test-integration vet lint fmt fmt-check vuln cover check tools run validate image push \
	compose-up compose-down quadlet-install htmx-vendor clean

build:
	CGO_ENABLED=0 go build -tags $(GOTAGS) -ldflags "$(LDFLAGS)" -o bin/mcp-machine ./cmd/mcp-machine

test:
	go test -tags $(GOTAGS) -count=1 ./...

test-race:
	go test -tags $(GOTAGS) -race -count=1 ./...

test-integration:
	go test -tags $(GOTAGS),integration -count=1 ./test/integration/...

vet:
	go vet -tags $(GOTAGS) ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found on PATH or in $(GOBIN_DIR); run 'make tools' to install it" >&2; exit 1; }
	golangci-lint run ./...

vuln:
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "govulncheck not found on PATH or in $(GOBIN_DIR); run 'make tools' to install it" >&2; exit 1; }
	govulncheck -tags $(GOTAGS) ./...

cover:
	go test -tags $(GOTAGS) -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1
	go tool cover -html=coverage.out -o coverage.html

# What CI runs locally, minus the container scan.
check: fmt-check lint vuln test-race

tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

fmt:
	gofmt -w cmd internal test

fmt-check:
	@out="$$(gofmt -l cmd internal test)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

run: build
	./bin/mcp-machine serve

validate: build
	./bin/mcp-machine validate

image:
	$(ENGINE) build \
		--build-arg MCPM_VERSION=$(TAG) --build-arg MCPM_COMMIT=$(COMMIT) --build-arg MCPM_DATE=$(DATE) \
		-t $(IMAGE):$(TAG) -t $(IMAGE):latest -f Containerfile .

push: image
	$(ENGINE) push $(IMAGE):$(TAG)

compose-up:
	$(ENGINE) compose up -d

compose-down:
	$(ENGINE) compose down

quadlet-install:
	mkdir -p $(QUADLET_DIR)
	cp deploy/quadlet/*.container deploy/quadlet/*.network $(QUADLET_DIR)/
	$(SYSTEMCTL) daemon-reload

htmx-vendor:
	mkdir -p $(dir $(HTMX_FILE))
	curl -fsSL -o $(HTMX_FILE) $(HTMX_URL)
	@echo "$(HTMX_SHA256)  $(HTMX_FILE)" | shasum -a 256 -c -

clean:
	rm -rf bin coverage.out coverage.html
