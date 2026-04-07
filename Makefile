VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD   ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD)

BINARY_AGENT      = ai-ops-agent
BINARY_INSTRUMENT = ai-ops-agent-instrument
BINARY_INIT       = ai-ops-agent-init
BINARY_UNINSTALL  = ai-ops-agent-uninstall

GO = CGO_ENABLED=1 go

.PHONY: all build clean test vet lint install release docker help

## ─── Quick Start ──────────────────────────────────────────
help: ## Show this help
	@echo ""
	@echo "  AI Ops Agent - Build Commands"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""

## ─── Build ────────────────────────────────────────────────
all: build ## Build everything

build: ## Build all binaries for current platform
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_AGENT) ./cmd/agent/
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_INSTRUMENT) ./cmd/instrument/
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_INIT) ./cmd/init/
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_UNINSTALL) ./cmd/uninstall/
	@echo "\n  Built: bin/$(BINARY_AGENT) bin/$(BINARY_INSTRUMENT) bin/$(BINARY_INIT) bin/$(BINARY_UNINSTALL)"

build-linux-amd64: ## Build for Linux amd64
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_AGENT)-linux-amd64 ./cmd/agent/
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_INSTRUMENT)-linux-amd64 ./cmd/instrument/

build-linux-arm64: ## Build for Linux arm64
	GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc $(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_AGENT)-linux-arm64 ./cmd/agent/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_INSTRUMENT)-linux-arm64 ./cmd/instrument/

build-all: build-linux-amd64 build-linux-arm64 ## Build for all platforms

## ─── Quality ──────────────────────────────────────────────
test: ## Run tests
	go test -v -race -count=1 ./...

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

check: vet test ## Run all checks

## ─── Install / Deploy ─────────────────────────────────────
install: build ## Install to /usr/local/bin
	sudo cp bin/$(BINARY_AGENT) /usr/local/bin/
	sudo cp bin/$(BINARY_INSTRUMENT) /usr/local/bin/
	sudo cp bin/$(BINARY_INIT) /usr/local/bin/
	@echo "  Installed to /usr/local/bin/"

## ─── Docker ───────────────────────────────────────────────
docker: ## Build Docker image
	docker build -t ai-ops-agent:$(VERSION) .
	docker tag ai-ops-agent:$(VERSION) ai-ops-agent:latest
	@echo "  Image: ai-ops-agent:$(VERSION)"

docker-run: ## Run in Docker
	docker run -d --name ai-ops-agent \
		-p 9090:9090 \
		-v $(PWD)/config.yaml:/app/config.yaml \
		ai-ops-agent:latest

## ─── Release ──────────────────────────────────────────────
release: check build-all ## Build release binaries
	@mkdir -p release
	cp bin/*-linux-* release/
	cp install.sh release/
	cp config.yaml release/config.example.yaml
	cd release && sha256sum * > checksums.txt
	@echo "\n  Release artifacts in release/"

## ─── Clean ────────────────────────────────────────────────
clean: ## Remove build artifacts
	rm -rf bin/ release/
	go clean -cache
