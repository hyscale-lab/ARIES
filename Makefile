.PHONY: build test test-race lint integration run setup setup-tool image-bridge charts-lint charts-template

PROFILE ?= profiles/openclaw-tb2-fix-git-deepseek.json

export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod

build:
	mkdir -p bin
	go build -o bin/aries ./cmd/aries
	CGO_ENABLED=0 go build -o bin/aries-ssh ./cmd/aries-ssh
	CGO_ENABLED=0 go build -o bin/aries-bridge ./cmd/aries-bridge

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
	go build -o bin/aries-setup ./k8s/setup/aries-setup
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/aries-setup-linux-amd64 ./k8s/setup/aries-setup

# The tool bridge pod's image. Only bridge changes need it rebuilt: the runner
# and its profiles live on the runner host (aries-setup setup_runner).
BRIDGE_IMAGE ?= aries-bridge:latest
BRIDGE_PLATFORM ?= linux/amd64

image-bridge:
	docker buildx build --platform $(BRIDGE_PLATFORM) -f Dockerfile.bridge -t $(BRIDGE_IMAGE) --load .

# Render the charts without a cluster. Catches template errors and, with
# --dry-run, schema problems that only show up once values are merged.
# HELM is the helm binary to use; override it if yours is not on PATH.
HELM ?= helm
# kube-prometheus-stack is pulled from its registry, pinned in prom_config.json.
PROM_CHART = $(shell jq -r '.chart_ref + ":" + .chart_version + "@" + .chart_digest' k8s/setup/configs/prometheus/prom_config.json)

charts-lint:
	$(HELM) lint ./k8s/aries

charts-template:
	$(HELM) template aries ./k8s/aries -n aries \
	  -f ./k8s/aries/values-local.yaml >/dev/null
	$(HELM) template aries ./k8s/aries -n aries \
	  -f ./k8s/aries/values-cluster.yaml --set registry.dockerconfigjson='{}' >/dev/null
	$(HELM) template prometheus $(PROM_CHART) -n monitoring \
	  -f ./k8s/prometheus/values.yaml -f ./k8s/grafana/values.yaml >/dev/null
	@echo "charts render"
