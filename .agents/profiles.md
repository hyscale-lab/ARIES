# Profiles and credentials

Start from `profiles/*.json`; consult the [schema](../pkg/config/config.go),
[configuration reference](../docs/configuration.md), and [support matrix](../docs/supported.md).
Use `configs/versions.json` for pins; do not duplicate the field inventory here.
Hermes derives its local image from the catalog’s base image and OTel plugin pins.

- Harness selection and deployment selection are separate. Currently harness and
  sandbox require the same local Docker daemon; only embedded bridges work.
  Reject unsupported placement before effects; do not imply Kubernetes support.
- Declare `overrides_file` explicitly (`""` disables overrides). Keep harness and
  sandbox resource limits independent; preserve omission/default semantics.
- Match model IDs to the served catalog. Keep credential values out of profiles,
  metadata, logs, and results; use configured environment-variable references.
- The ignored root `DEEPSEEK_API.key` must be a private, current-user-owned regular
  file, never a symlink. Invalid files fail closed; absence allows env fallback.
- Remove model credentials from managed SGLang's child environment. Use HTTPS for
  remote/credentialed endpoints; local HTTP uses a non-secret placeholder. URL
  validation does not enforce this [transport policy](../docs/implementation/model-runtime.md#implementation-gap-http-transport-policy).
- `aries setup PROFILE` only prepares pinned data/images; it must not contact
  model endpoints or start a model runtime. `aries PROFILE` runs the experiment.

Usage: [quick start](../docs/quick-start.md), [runtime contract](../docs/design/runtime.md).
