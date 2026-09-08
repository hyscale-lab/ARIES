.PHONY: build test test-race lint integration run setup proto

PROFILE ?= profiles/openclaw-tb2-fix-git-deepseek.json

export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod

TOOLS := $(CURDIR)/.cache/tools

build:
	mkdir -p bin
	go build -o bin/aries ./cmd/aries
	CGO_ENABLED=0 go build -o bin/aries-ssh ./cmd/aries-ssh

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
	ARIES_SSH_CLIENT=$(CURDIR)/.cache/integration/aries-ssh go test -p=1 -count=1 -tags=integration ./...

# Regenerate the protobuf and gRPC bindings. Standalone on purpose: the
# generated code is committed, so build, test, test-race, lint and integration
# all work on a clean checkout with no protobuf toolchain present. Run this
# only after editing a .proto, then commit the result.
proto:
	mkdir -p $(TOOLS)
	go build -o $(TOOLS)/buf github.com/bufbuild/buf/cmd/buf
	go build -o $(TOOLS)/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	go build -o $(TOOLS)/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc
	cd pkg/bridge/hermesgrpc && PATH="$(TOOLS):$$PATH" $(TOOLS)/buf lint
	cd pkg/bridge/hermesgrpc && PATH="$(TOOLS):$$PATH" $(TOOLS)/buf generate

run: build
	./bin/aries $(PROFILE)

# Optional prewarm: validate the profile and prepare benchmark/image inputs
# without contacting or starting the configured model service.
setup: build
	./bin/aries setup $(PROFILE)
