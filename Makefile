.PHONY: build test test-race lint integration

export GOCACHE ?= $(CURDIR)/.cache/go-build

build:
	mkdir -p bin
	go build -o bin/aries ./cmd/aries

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	test -z "$$(gofmt -l $$(find cmd pkg -name '*.go' -type f))"
	go vet ./...

integration:
	go test -tags=integration ./...
