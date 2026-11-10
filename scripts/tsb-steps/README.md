# Native OpenClaw metadata patch

This directory contains the source patch and version manifest needed by the
[VMM-owned tracer](../../docs/experiments/memory-footprint-tracing.md).
[`versions.json`](versions.json) pins upstream OpenClaw
`2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4` (`v2026.7.1`), the patch SHA-256 and
the resulting eight source/test file hashes. [`native-openclaw.patch`](native-openclaw.patch)
was regenerated from the modified worker checkout to preserve those files
exactly. The active ARIES plugin is
[`scripts/tsb-access-dirty-plugin`](../tsb-access-dirty-plugin); this directory
contains no plugin or diagnostic collector to stage.

The patch adds source dispatch/response clocks, returned tool IDs, call category
and native background-process lifecycle metadata. It retains source IDs without
changing shell bodies, tool arguments, scheduling or process control. Model
stream-call IDs do not certify every provider HTTP retry; raw model calls and
primary agent-chain calls remain distinct. Process-session registration occurs
before spawn, and exit records describe the native process/transport lifetime,
not every detached guest descendant. These limits also apply to a successfully
rebuilt image.

## Reconstruct and validate the source

From the ARIES repository root, use a new destination and a clean checkout of
the pinned upstream base. Apply the patch once:

```sh
aries_root=$PWD
git clone https://github.com/openclaw/openclaw.git .cache/openclaw-tracer
cd .cache/openclaw-tracer
git checkout --detach 2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4
git apply --check "$aries_root/scripts/tsb-steps/native-openclaw.patch"
git apply "$aries_root/scripts/tsb-steps/native-openclaw.patch"
git diff --check
```

Check the patch and resulting file hashes against `versions.json` when moving
the source to another machine. Applying successfully alone does not validate a
native runtime build or its delivered hooks.

Use the upstream pin's Node 24 toolchain, pnpm 11.2.2 and frozen lockfile. The
patch includes tests in two existing upstream test files:

```sh
corepack pnpm install --frozen-lockfile
corepack pnpm exec vitest run --config test/vitest/vitest.agents-embedded-agent.config.ts \
  src/agents/embedded-agent-runner/run/attempt.model-diagnostic-events.test.ts --maxWorkers=1
corepack pnpm exec vitest run --config test/vitest/vitest.agents-core.config.ts \
  src/agents/bash-process-registry.test.ts --maxWorkers=1
```

## Build and select the native image

From that patched checkout, use its upstream Dockerfile and native postbuild
steps:

```sh
sudo docker build --build-arg OPENCLAW_DOCKER_BUILD_SKIP_DTS=1 \
  --build-arg OPENCLAW_DOCKER_BUILD_NODE_OPTIONS=--max-old-space-size=8192 \
  -t aries/openclaw:2026.7.1-tsb-metadata .
```

Record the source, patch and built image digests, and select that image explicitly
in the experiment's versions file. Staging an ARIES plugin does not modify a
published native image. Validate native diagnostic delivery, batch boundaries,
terminal release and cancellation with deterministic fixtures on the selected
runtime before interpreting tracer measurements. The source-only cleanup does
not itself build an image or run an experiment.
