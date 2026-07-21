.PHONY: build test test-race lint integration setup-terminalbench

export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod

build:
	mkdir -p bin
	go build -o bin/aries ./cmd/aries
	CGO_ENABLED=0 go build -o bin/aries-exec-helper ./cmd/aries-exec-helper

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	test -z "$$(gofmt -l $$(find cmd pkg -name '*.go' -type f))"
	go vet ./...

integration:
	mkdir -p .cache/integration
	CGO_ENABLED=0 go build -o .cache/integration/aries-exec-helper ./cmd/aries-exec-helper
	ARIES_EXEC_HELPER=$(CURDIR)/.cache/integration/aries-exec-helper go test -count=1 -tags=integration ./...

setup-terminalbench:
	go run ./cmd/aries setup terminalbench2
