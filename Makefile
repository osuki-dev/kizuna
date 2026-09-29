BINARY_NAME := kizuna
BUILD_DIR := bin
MODULE := github.com/osuki-dev/kizuna

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -s -w \
	-X 'main.version=$(VERSION)' \
	-X 'main.commit=$(COMMIT)' \
	-X 'main.buildTime=$(BUILD_TIME)'

.PHONY: all build test test-coverage lint tidy clean install cross-compile help run

all: lint test build

## build: Compile kizuna binary for the host platform
build:
	@echo "==> Building $(BINARY_NAME) ($(VERSION))"
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="$(LDFLAGS)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/kizuna
	@echo "==> Successfully built $(BUILD_DIR)/$(BINARY_NAME)"

## test: Run unit tests with race detection
test:
	@echo "==> Running unit tests"
	go test -v -race ./...

## test-coverage: Run tests and generate HTML coverage report
test-coverage:
	@echo "==> Running tests with coverage"
	@mkdir -p $(BUILD_DIR)
	go test -race -coverprofile=$(BUILD_DIR)/coverage.out ./...
	go tool cover -html=$(BUILD_DIR)/coverage.out -o $(BUILD_DIR)/coverage.html
	@echo "==> Coverage report written to $(BUILD_DIR)/coverage.html"

## lint: Check code formatting and static analysis
lint:
	@echo "==> Running static analysis (go vet)"
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		echo "==> Running golangci-lint"; \
		golangci-lint run ./...; \
	fi

## tidy: Format code and clean unused go dependencies
tidy:
	@echo "==> Tidying dependencies"
	go mod tidy
	@echo "==> Formatting Go files"
	go fmt ./...

## clean: Remove build artifacts and temporary files
clean:
	@echo "==> Cleaning build artifacts"
	rm -rf $(BUILD_DIR) $(BINARY_NAME)
	@echo "==> Done"

## install: Install binary to GOPATH/bin
install: build
	@echo "==> Installing $(BINARY_NAME) to $(shell go env GOPATH)/bin"
	cp $(BUILD_DIR)/$(BINARY_NAME) $(shell go env GOPATH)/bin/

## cross-compile: Cross-compile binaries for Linux, macOS, and Windows
cross-compile:
	@echo "==> Cross-compiling for Linux (amd64, arm64)"
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/kizuna
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/kizuna
	@echo "==> Cross-compiling for macOS (amd64, arm64)"
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/kizuna
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/kizuna
	@echo "==> Cross-compiling for Windows (amd64)"
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/kizuna
	@echo "==> Cross-compilation complete! Binaries located in $(BUILD_DIR)/"

## package: Package cross-compiled release archives and generate sha256 checksums
package: cross-compile
	@echo "==> Packaging release archives into dist/"
	@mkdir -p dist
	@tar -czf dist/$(BINARY_NAME)_$(VERSION)_linux_amd64.tar.gz -C $(BUILD_DIR) $(BINARY_NAME)-linux-amd64
	@tar -czf dist/$(BINARY_NAME)_$(VERSION)_linux_arm64.tar.gz -C $(BUILD_DIR) $(BINARY_NAME)-linux-arm64
	@tar -czf dist/$(BINARY_NAME)_$(VERSION)_darwin_amd64.tar.gz -C $(BUILD_DIR) $(BINARY_NAME)-darwin-amd64
	@tar -czf dist/$(BINARY_NAME)_$(VERSION)_darwin_arm64.tar.gz -C $(BUILD_DIR) $(BINARY_NAME)-darwin-arm64
	@if command -v zip >/dev/null 2>&1; then \
		(cd $(BUILD_DIR) && zip ../dist/$(BINARY_NAME)_$(VERSION)_windows_amd64.zip $(BINARY_NAME)-windows-amd64.exe); \
	fi
	@cd dist && sha256sum *.* > checksums.txt && cp checksums.txt sha256sums.txt
	@echo "==> Successfully packaged archives and generated dist/checksums.txt"

## help: Display available make targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Available targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":"}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
