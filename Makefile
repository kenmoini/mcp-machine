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

.PHONY: build test test-race test-integration lint fmt fmt-check run validate image push \
	compose-up compose-down quadlet-install htmx-vendor clean

build:
	CGO_ENABLED=0 go build -tags $(GOTAGS) -ldflags "$(LDFLAGS)" -o bin/mcp-machine ./cmd/mcp-machine

test:
	go test -tags $(GOTAGS) -count=1 ./...

test-race:
	go test -tags $(GOTAGS) -race -count=1 ./...

test-integration:
	go test -tags $(GOTAGS),integration -count=1 ./test/integration/...

lint:
	go vet -tags $(GOTAGS) ./... && go tool staticcheck -tags $(GOTAGS) ./...

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
		--build-arg VERSION=$(TAG) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
		-t $(IMAGE):$(TAG) -f Containerfile .

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
	rm -rf bin
