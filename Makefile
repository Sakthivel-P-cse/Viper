TOOLS_DIR ?= .tools/bin

BUF_VERSION ?= v1.72.0
VULNCHECK_VERSION ?= latest
GITLEAKS_VERSION ?= latest

BUF ?= $(TOOLS_DIR)/buf
GOVULNCHECK := $(TOOLS_DIR)/govulncheck
GITLEAKS := $(TOOLS_DIR)/gitleaks

.PHONY: fmt fmt-check proto-check test vet verify tools security-check

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

tools:
	mkdir -p $(TOOLS_DIR)
	GOBIN="$(abspath $(TOOLS_DIR))" go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	GOBIN="$(abspath $(TOOLS_DIR))" go install golang.org/x/vuln/cmd/govulncheck@$(VULNCHECK_VERSION)
	GOBIN="$(abspath $(TOOLS_DIR))" go install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)

security-check: tools
	$(GOVULNCHECK) ./...
	$(GITLEAKS) detect --source . --redact --no-banner
