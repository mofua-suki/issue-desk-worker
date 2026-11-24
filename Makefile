GO ?= go
GOFMT ?= gofmt
REVISION ?= development

.PHONY: check build
check:
	GO="$(GO)" GOFMT="$(GOFMT)" sh scripts/verify.sh -race
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.revision=$(REVISION)" -o bin/worker ./cmd/worker
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o bin/desk ./cmd/desk
