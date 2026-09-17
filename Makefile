.PHONY: build test test-race lint integration run setup setup-tool charts-lint charts-template

PROFILE ?= profiles/openclaw-tb2-fix-git-deepseek.json

export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod

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

run: build
	./bin/aries $(PROFILE)

# Optional prewarm: validate the profile and prepare benchmark/image inputs
# without contacting or starting the configured model service.
setup: build
	./bin/aries setup $(PROFILE)

# Cluster setup tool: a host binary for create_cluster, and a static linux
# binary to copy onto nodes when running the on-node subcommands by hand.
setup-tool:
	mkdir -p bin
	go build -o bin/aries-setup ./setup/aries-setup
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/aries-setup-linux-amd64 ./setup/aries-setup

# Render the charts without a cluster. Catches template errors and, with
# --dry-run, schema problems that only show up once values are merged.
# HELM is the helm binary to use; override it if yours is not on PATH.
HELM ?= helm

charts-lint:
	$(HELM) lint ./k8s/aries --set model.apiKey=lint-only
	$(HELM) lint ./k8s/prometheus/chart -f ./k8s/prometheus/values.yaml -f ./k8s/grafana/values.yaml

charts-template:
	$(HELM) template aries ./k8s/aries -n aries \
	  -f ./k8s/aries/values-local.yaml --set model.apiKey=template-only >/dev/null
	$(HELM) template aries ./k8s/aries -n aries \
	  -f ./k8s/aries/values-incluster.yaml --set model.apiKey=template-only \
	  --set registry.dockerconfigjson='{}' >/dev/null
	$(HELM) template prometheus ./k8s/prometheus/chart -n monitoring \
	  -f ./k8s/prometheus/values.yaml -f ./k8s/grafana/values.yaml >/dev/null
	@echo "charts render"
