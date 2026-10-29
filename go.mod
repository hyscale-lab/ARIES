module github.com/hyscale-lab/aries

go 1.26.5

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/containerd/containerd v1.7.33
	github.com/containerd/containerd/api v1.8.0
	github.com/containerd/errdefs v1.0.0
	github.com/containerd/ttrpc v1.2.7
	github.com/containernetworking/cni v1.3.0
	github.com/distribution/reference v0.6.0
	github.com/firecracker-microvm/firecracker-containerd v0.0.0-20260713114451-bb007a1ef569
	github.com/firecracker-microvm/firecracker-go-sdk v1.0.1-0.20250818195323-ed6ff32aa924
	github.com/gorilla/websocket v1.5.3
	github.com/moby/moby/api v1.56.0
	github.com/moby/moby/client v0.6.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/runtime-spec v1.2.0
	github.com/parquet-go/parquet-go v0.32.0
	github.com/sirupsen/logrus v1.10.0
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
)

require (
	cyphar.com/go-pathrs v0.2.1 // indirect
	github.com/AdaLogics/go-fuzz-headers v0.0.0-20230811130428-ced1acdcaa24 // indirect
	github.com/AdamKorcz/go-118-fuzz-build v0.0.0-20230306123547-8075edf89bb0 // indirect
	github.com/Microsoft/hcsshim v0.12.9 // indirect
	github.com/containerd/cgroups/v3 v3.0.3 // indirect
	github.com/containerd/console v1.0.5 // indirect
	github.com/containerd/continuity v0.4.4 // indirect
	github.com/containerd/fifo v1.1.0 // indirect
	github.com/containerd/go-runc v1.0.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/containerd/platforms v0.2.1 // indirect
	github.com/containerd/typeurl/v2 v2.2.0 // indirect
	github.com/containernetworking/plugins v1.7.1 // indirect
	github.com/cyphar/filepath-securejoin v0.6.0 // indirect
	github.com/docker/go-events v0.0.0-20190806004212-e31b211e4f1c // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/mdlayher/socket v0.5.1 // indirect
	github.com/mdlayher/vsock v1.2.1 // indirect
	github.com/moby/locker v1.0.1 // indirect
	github.com/moby/sys/mountinfo v0.7.1 // indirect
	github.com/moby/sys/sequential v0.5.0 // indirect
	github.com/moby/sys/signal v0.7.0 // indirect
	github.com/moby/sys/user v0.3.0 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/opencontainers/selinux v1.13.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.opencensus.io v0.24.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	google.golang.org/genproto v0.0.0-20240123012728-ef4313101c80 // indirect
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/andybalholm/brotli v1.1.1 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/parquet-go/bitpack v1.0.0 // indirect
	github.com/parquet-go/jsonlite v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/twpayne/go-geom v1.6.1 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.60.0 // indirect
	go.opentelemetry.io/otel v1.35.0 // indirect
	go.opentelemetry.io/otel/metric v1.35.0 // indirect
	go.opentelemetry.io/otel/trace v1.35.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
	google.golang.org/grpc v1.71.1
	google.golang.org/protobuf v1.36.6
)

replace github.com/firecracker-microvm/firecracker-containerd => github.com/vhive-serverless/firecracker-containerd v0.0.0-20260713114451-bb007a1ef569

replace github.com/firecracker-microvm/firecracker-go-sdk => github.com/vhive-serverless/firecracker-go-sdk v0.0.0-20230910093528-55ab6c76ad40
