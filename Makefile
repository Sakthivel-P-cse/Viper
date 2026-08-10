TOOLS_DIR ?= .tools/bin

BUF_VERSION ?= v1.72.0
VULNCHECK_VERSION ?= v1.6.0
GITLEAKS_VERSION ?= v8.30.1

BUF ?= $(TOOLS_DIR)/buf
GOVULNCHECK := $(TOOLS_DIR)/govulncheck
GITLEAKS := $(TOOLS_DIR)/gitleaks

.PHONY: fmt fmt-check proto-check test vet verify tools security-check build build-release demo compose-tls compose-up compose-down

fmt:
	gofmt -w $$(rg --files -g '*.go')

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

proto-check:
	@mkdir -p /tmp/viper-proto
	$(BUF) lint
	$(BUF) build --as-file-descriptor-set --output=/tmp/viper-proto/contracts.binpb
	go run ./cmd/viper-contractcheck < /tmp/viper-proto/contracts.binpb

test:
	go test -race ./...

vet:
	go vet ./...

verify: fmt-check proto-check vet test

build:
	go build ./...

VERSION ?= dev
COMMIT ?= unknown
BUILD_DATE ?= unknown

build-release:
	mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.buildCommit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)" -o dist/viper ./cmd/viper

demo:
	go run ./cmd/viper demo

compose-tls:
	sh deploy/compose/generate-dev-tls.sh

compose-up:
	docker compose -f deploy/compose/docker-compose.yml up --build -d

compose-down:
	docker compose -f deploy/compose/docker-compose.yml down

tools:
	mkdir -p $(TOOLS_DIR)
	@if [ ! -x "$(BUF)" ]; then GOBIN="$(abspath $(TOOLS_DIR))" go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION); fi
	@if [ ! -x "$(GOVULNCHECK)" ]; then GOBIN="$(abspath $(TOOLS_DIR))" go install golang.org/x/vuln/cmd/govulncheck@$(VULNCHECK_VERSION); fi
	@if [ ! -x "$(GITLEAKS)" ]; then GOBIN="$(abspath $(TOOLS_DIR))" go install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION); fi

security-check: tools
	$(GOVULNCHECK) ./...
	$(GITLEAKS) detect --source . --redact --no-banner
