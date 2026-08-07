BUF ?= buf

.PHONY: fmt fmt-check proto-check test vet verify

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
