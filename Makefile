.PHONY: build test test-race lint integration setup-terminalbench

export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod

build:
	mkdir -p bin
	go build -o bin/aries ./cmd/aries
	CGO_ENABLED=0 go build -o bin/aries-exec-helper ./cmd/aries-exec-helper
	CGO_ENABLED=0 go build -o bin/aries-ssh ./cmd/aries-ssh
	CGO_ENABLED=0 go build -o bin/aries-ssh-server ./cmd/aries-ssh-server

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
	CGO_ENABLED=0 go build -o .cache/integration/aries-ssh ./cmd/aries-ssh
	CGO_ENABLED=0 go build -o .cache/integration/aries-ssh-server ./cmd/aries-ssh-server
	ARIES_EXEC_HELPER=$(CURDIR)/.cache/integration/aries-exec-helper ARIES_SSH_CLIENT=$(CURDIR)/.cache/integration/aries-ssh ARIES_SSH_SERVER=$(CURDIR)/.cache/integration/aries-ssh-server go test -p=1 -count=1 -tags=integration ./...

setup-terminalbench:
	go run ./cmd/aries setup terminalbench2
