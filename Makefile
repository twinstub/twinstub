BINARY := twinstub
LDFLAGS := -s -w \
	-X github.com/twinstub/twinstub/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
	-X github.com/twinstub/twinstub/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo none) \
	-X github.com/twinstub/twinstub/internal/version.Date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: build test lint vet cover bench clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/twinstub

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

cover:
	go test -coverpkg=./internal/... -coverprofile=cover.out ./internal/...
	go tool cover -func=cover.out | tail -1

bench:
	go test -bench=BenchmarkStatelessReply -benchmem -run=^$$ ./internal/server/

clean:
	rm -rf bin dist cover.out
