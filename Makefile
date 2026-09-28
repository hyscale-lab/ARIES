.PHONY: build test test-race lint integration run setup

PROFILE ?= profiles/openclaw-tb2-fix-git-deepseek.json

export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod

build:
	mkdir -p bin
	go build -o bin/aries ./cmd/aries
	CGO_ENABLED=0 go build -o bin/aries-ssh ./cmd/aries-ssh
	CGO_ENABLED=0 go build -o bin/aries-codex-ssh ./cmd/aries-codex-ssh
	CGO_ENABLED=0 go build -o bin/aries-codex-exec ./cmd/aries-codex-exec

test:
	go test -v ./...

test-race:
	go test -race ./...

lint:
	test -z "$$(gofmt -l $$(find cmd internal pkg -name '*.go' -type f))"
	go vet ./...

integration:
	mkdir -p .cache/integration
	CGO_ENABLED=0 go build -o .cache/integration/aries-ssh ./cmd/aries-ssh
	CGO_ENABLED=0 go build -o .cache/integration/aries-codex-ssh ./cmd/aries-codex-ssh
	CGO_ENABLED=0 go build -o .cache/integration/aries-codex-exec ./cmd/aries-codex-exec
	ARIES_SSH_CLIENT=$(CURDIR)/.cache/integration/aries-ssh ARIES_CODEX_SSH_CLIENT=$(CURDIR)/.cache/integration/aries-codex-ssh ARIES_CODEX_EXEC_SUPERVISOR=$(CURDIR)/.cache/integration/aries-codex-exec go test -p=1 -count=1 -tags=integration ./...

run: build
	./bin/aries $(PROFILE)

# Optional prewarm: validate the profile and prepare benchmark/image inputs
# without contacting or starting the configured model service.
setup: build
	./bin/aries setup $(PROFILE)
