.PHONY: build build-mac run tidy vet fmt test clean install

NATIVE_GOOS := $(shell go env GOOS 2>/dev/null || echo darwin)
NATIVE_GOARCH := $(shell go env GOARCH 2>/dev/null || echo arm64)

BINARY  := lazyloader
PKG     := github.com/egomarker/docker-lazyloader
CMD     := ./cmd/lazyloader
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# macOS arm64 (Apple Silicon) by default. Override: make build GOOS=darwin GOARCH=amd64
GOOS ?= darwin
GOARCH ?= arm64

LDFLAGS := -X main.version=$(VERSION)

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

# Cross-compile from any host (Linux dev box -> macOS Mac).
build-mac: build

run:
	GOOS=$(NATIVE_GOOS) GOARCH=$(NATIVE_GOARCH) go run $(CMD) -config examples/lazyloader.example.yaml

tidy:
	go mod tidy

vet:
	go vet ./...

fmt:
	gofmt -s -w .

test:
	go test ./...

install: build
	mkdir -p $(HOME)/bin
	cp $(BINARY) $(HOME)/bin/$(BINARY)

clean:
	rm -f $(BINARY)
