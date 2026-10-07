# Profiles and credentials

Start from `profiles/*.json`; consult the [schema](../pkg/config/config.go),
[configuration reference](../docs/configuration.md), and [support matrix](../docs/supported.md).
Use `configs/versions.json` for pins; do not duplicate the field inventory here.

- Keep role selection separate from placement. Harness and sandbox share a local
  Docker daemon; bridges use a separate managed Docker container.
  Reject unsupported backends and explicit embedded mode before effects.
- Declare `overrides_file` explicitly (`""` disables overrides). Keep harness and
  sandbox resource limits independent; preserve omission/default semantics and
  explicit model request settings without silently changing their meaning. Judges
  share model fields; preserve independent evaluation defaults and validation.
- Match model IDs to the served catalog. Keep credential values out of profiles,
  metadata, logs, and results; use configured environment-variable references.
- Preserve [DeepSeek credential-file validation and fallback](../docs/configuration.md#external-deepseek).
- Remove model credentials from managed SGLang's child environment. Use HTTPS for
  remote/credentialed endpoints; local HTTP uses a non-secret placeholder. URL
  validation does not enforce this [transport policy](../docs/implementation/model-runtime.md#implementation-gap-http-transport-policy).
- `aries setup PROFILE` only prepares pinned data/images; it must not contact
  model endpoints or start a model runtime. `aries PROFILE` runs the experiment.

`profiles/experiments/` holds the paper-reproduction profiles; their settings are
documented in [configuration](../docs/configuration.md#paper-reproduction-profiles).

Human references: [configuration](../docs/configuration.md),
[quick start](../docs/quick-start.md),
[runtime](../docs/design/runtime.md), [support matrix](../docs/supported.md).
