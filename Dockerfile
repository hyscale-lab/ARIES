# Build ARIES in the official Go image, then run it in a slim Debian image that
# carries only the runtime tools the host container needs. The OpenClaw agent
# and task sandboxes are spawned as sibling containers via the mounted Docker
# socket, so this image intentionally does not bundle Node or the benchmark
# images.
FROM golang:1.26 AS builder
WORKDIR /app
COPY . .
RUN make build

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
