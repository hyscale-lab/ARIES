# Profiles and model runtimes

Start from an existing `profiles/*.json`. Schema: [config.go](../pkg/config/config.go);
version pins: `configs/versions.json`; runtime/preflight: `internal/app`.

- `name` identifies the experiment; `benchmark.type`, `root`, and `tasks` select
  pinned input and task occurrences. `versions_file` selects pins; `output_dir`
  holds private artifacts. `execution.concurrency` caps admitted work;
  `arrivals_file` with `arrival_rate_per_min` optionally schedules arrivals.
- `harness.type` selects Hermes or OpenClaw independently of deployment.
  `harness.deployment` and `sandbox.deployment` currently require Docker on the
  same local Unix socket. Kubernetes is recognized but rejected before effects.
- `bridge.mode` supports only `embedded`; use the matching harness/bridge pair.
- Declare `overrides_file` explicitly; `""` disables overrides. Harness resource
  limits and sandbox resource limits are independent. Omitted harness dimensions
  stay unlimited; omitted sandbox dimensions retain benchmark values.
- `runtime.backend` selects endpoint behavior; `runtime.mode` selects ownership.
  DeepSeek and generic `openai` are external only. SGLang can be external or
  managed; managed configuration validates native YAML model, port, and GPU
  selection before starting one host process for the run.
- `model.id` must match the served model. DeepSeek Flash profiles use
  `deepseek-flash`; preflight checks the live catalog. Do not put keys in JSON.
- Official DeepSeek can load ignored root `DEEPSEEK_API.key`: owner-readable,
  current-user-owned regular file, no symlink or group/world permissions.
  An invalid existing file fails closed; absence permits environment fallback.
  Other credentials come from their configured environment variable names.
- Remove the configured credential from managed SGLang's child environment.
  Use HTTPS for remote/credentialed endpoints; trusted local HTTP uses a
  non-secret placeholder. This is operator policy; URL validation does not yet
  enforce locality (see [transport gap](../docs/implementation/model-runtime.md#implementation-gap-http-transport-policy)).

`./bin/aries PROFILE.json` prepares inputs and runs. `./bin/aries setup
PROFILE.json` only prewarms pinned data/images; it neither contacts model endpoints
nor starts a runtime. ARIES does not install SGLang or download model weights.

Human references: [configuration](../docs/configuration.md),
[quick start](../docs/quick-start.md),
[runtime](../docs/design/runtime.md), [support matrix](../docs/supported.md).
