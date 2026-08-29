# Build ARIES in the official Go image, then run it in a slim Debian image that
# carries only the runtime tools the host container needs. The OpenClaw agent
# and task sandboxes are spawned as sibling containers via the mounted Docker
# socket, so this image intentionally does not bundle Node or the benchmark
# images.
# Run the builder on the host's native architecture and cross-compile to the
# target arch. This avoids emulating the Go compiler (which hangs under qemu),
# so building a linux/amd64 image on an arm64 Mac stays fast.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} make build

FROM debian:bookworm-slim
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates docker.io openssh-client curl \
    && curl -fsSL "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/$(dpkg --print-architecture)/kubectl" -o /usr/local/bin/kubectl \
    && chmod +x /usr/local/bin/kubectl \
    && apt-get purge -y curl && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/bin/aries /app/bin/aries
COPY --from=builder /app/bin/aries-ssh /app/bin/aries-ssh
COPY profiles/ profiles/
COPY traces/ traces/
COPY configs/ configs/
COPY docs/ docs/
COPY LICENSE LICENSE
COPY LICENSE-CODE LICENSE-CODE
COPY README.md README.md
ENTRYPOINT ["/app/bin/aries"]
# Default to the OpenClaw E2B-bridge profile so `docker run` tries the E2B path.
CMD ["profiles/openclaw-tb2-fix-git-deepseek-e2b.json"]
