.PHONY: build test test-race lint integration run setup k8s-cluster k8s-images k8s-clean k8s-prune charts-lint charts-template

PROFILE ?= profiles/openclaw-tb2-fix-git-deepseek.json

export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod

build:
	mkdir -p bin
	go build -o bin/aries ./cmd/aries
	CGO_ENABLED=0 go build -o bin/aries-echo ./cmd/aries-echo
	CGO_ENABLED=0 go build -o bin/aries-ssh-client ./cmd/aries-ssh-client
	CGO_ENABLED=0 go build -o bin/aries-bridge ./cmd/aries-bridge

test:
	go test -v ./...

test-race:
	go test -race ./...

lint:
	test -z "$$(gofmt -l $$(find cmd internal pkg scripts -name '*.go' -type f))"
	go vet ./...

integration: bridge-image
	ARIES_SSH_CLIENT=$(CURDIR)/bin/aries-ssh-client ARIES_ECHO_SERVER=$(CURDIR)/bin/aries-echo go test -p=1 -count=1 -tags=integration ./...

run: build
	./bin/aries $(PROFILE)

# Optional prewarm: validate the profile and prepare benchmark/image inputs
# without contacting or starting the configured model service.
setup: build
	./bin/aries setup $(PROFILE)

# Compiler and plugins are pinned; generated bindings are checked into the tree.
PROTOC_VERSION := 29.3
PROTOC := $(CURDIR)/.cache/protobuf/bin/protoc
PROTOC_GEN_GO_VERSION := v1.36.6
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1
.PHONY: proto-tools proto
proto-tools:
	mkdir -p .cache/protobuf
	curl -fL -o .cache/protobuf/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip
	echo '3e866620c5be27664f3d2fa2d656b5f3e09b5152b42f1bedbf427b333e90021a  .cache/protobuf/protoc.zip' | sha256sum -c -
	unzip -qo .cache/protobuf/protoc.zip -d .cache/protobuf
	GOBIN=$(CURDIR)/.cache/protobuf/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(CURDIR)/.cache/protobuf/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

proto:
	test "$$($(CURDIR)/.cache/protobuf/bin/protoc-gen-go --version)" = 'protoc-gen-go $(PROTOC_GEN_GO_VERSION)'
	test "$$($(CURDIR)/.cache/protobuf/bin/protoc-gen-go-grpc --version)" = 'protoc-gen-go-grpc $(patsubst v%,%,$(PROTOC_GEN_GO_GRPC_VERSION))'
	test "$$($(PROTOC) --version)" = 'libprotoc $(PROTOC_VERSION)'
	PATH=$(CURDIR)/.cache/protobuf/bin:$$PATH $(PROTOC) --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pkg/bridge/control/v1/control.proto

.PHONY: bridge-image
bridge-image: build
	go run ./scripts/build-bridge-image

# Build a cluster with Invitro's setup and apply ARIES to it. NODES lists SSH
# targets: the master, the runner (Invitro's loader node), then the workers.
#   make k8s-cluster NODES="user@master user@runner user@worker1"
k8s-cluster:
	./k8s/create-cluster.sh $(K8S_FLAGS) $(NODES)

# Build the bridge and Hermes images for the cluster and push them. REGISTRY is
# a repository prefix; log in with docker login first.
#   make k8s-images REGISTRY=docker.io/user
#   make k8s-images REGISTRY=docker.io/user K8S_IMAGES_FLAGS="--kube-version 1.34.3"
k8s-images:
	./k8s/publish-images.sh $(K8S_IMAGES_FLAGS) $(REGISTRY)

# Remove the pods, services and NetworkPolicies an interrupted runner left
# behind. Every run's objects carry the same label, so this also stops any run
# still in progress in the namespace. The chart's own objects are kept.
#   make k8s-clean NAMESPACE=aries
NAMESPACE ?= aries
k8s-clean:
	kubectl -n $(NAMESPACE) delete pods,services,networkpolicies \
	  -l app.kubernetes.io/managed-by=aries --ignore-not-found --wait

# Remove what Invitro installs for its serverless experiments and ARIES does not
# use: Knative, Istio, MetalLB, vHive's registry and the pushgateway. Needs
# cluster-admin access. K8S_PRUNE_FLAGS="--dry-run" lists it without changing it.
#   make k8s-prune K8S_PRUNE_FLAGS="--dry-run"
k8s-prune:
	./k8s/prune-invitro.sh $(K8S_PRUNE_FLAGS)

# Render the chart without a cluster. Catches template errors and, with
# --dry-run, schema problems that only show up once values are merged.
# HELM is the helm binary to use; override it if yours is not on PATH.
HELM ?= helm
charts-lint:
	$(HELM) lint ./k8s/aries

charts-template:
	$(HELM) template aries ./k8s/aries -n aries \
	  -f ./k8s/aries/values-local.yaml >/dev/null
	$(HELM) template aries ./k8s/aries -n aries \
	  -f ./k8s/aries/values-cluster.yaml --set registry.dockerconfigjson='{}' >/dev/null
	@echo "charts render"
