# aimon — Makefile
# Parallel-by-default: tests use up to (nproc-1) cores.

BINARY      := aimon
PKG         := ./cmd/aimon
IMAGE       ?= ghcr.io/algonode/aimon
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TAG         ?= $(VERSION)
JOBS        := $(shell nproc --ignore=1 2>/dev/null || echo 1)
LDFLAGS     := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

## ---- local development ----------------------------------------------------

.PHONY: build
build: ## Build a static binary into ./bin
	CGO_ENABLED=0 GOFLAGS="-trimpath" go build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

.PHONY: run
run: build ## Build and run against ./config.yaml
	./bin/$(BINARY) --config config.yaml

.PHONY: dev
dev: ## Run with verbose logging (no build artifact)
	AIMON_LOG=debug go run -ldflags="$(LDFLAGS)" $(PKG) --config config.yaml

.PHONY: test
test: ## Run tests across (nproc-1) cores
	go test -p $(JOBS) -parallel $(JOBS) ./...

.PHONY: fmt
fmt: ## gofmt the tree
	gofmt -w .

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin

## ---- container ------------------------------------------------------------

.PHONY: image
image: ## Build the scratch image locally and load it into docker
	docker buildx build --platform linux/amd64 \
		--build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) --load .

.PHONY: docker
docker: ## Build + push a multi-attested image (SBOM + provenance)
	docker buildx build --sbom=true --provenance=true --platform linux/amd64 \
		--build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) -t $(IMAGE):latest --push .

.PHONY: sbom
sbom: image ## Print the SBOM of the locally built image (needs docker buildx + syft/scout)
	docker sbom $(IMAGE):$(TAG) 2>/dev/null || docker scout sbom $(IMAGE):$(TAG)

.PHONY: up
up: ## Start via docker-compose (builds the image, bind-mounts ./data)
	docker compose up -d --build

.PHONY: down
down: ## Stop the docker-compose stack
	docker compose down

.PHONY: logs
logs: ## Tail compose logs
	docker compose logs -f

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
