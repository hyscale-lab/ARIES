# Profiles and credentials

Start from `profiles/*.json`; consult the [schema](../pkg/config/config.go),
[configuration reference](../docs/configuration.md), and [support matrix](../docs/supported.md).
Use `configs/versions.json` for pins; do not duplicate the field inventory here.

- Keep role selection separate from placement. Docker components share one local
  daemon and run network. vHive tool sandboxes use the local Firecracker/containerd
  stack with Docker harnesses and a managed Docker bridge; see its
  [constraints](../docs/implementation/vhive.md). On Kubernetes all three
  components share one namespace and backend. Reject other mixed compositions
  and explicit embedded mode before effects.
- Declare `overrides_file` explicitly (`""` disables overrides). Keep harness and
  sandbox resource limits independent; preserve omission/default semantics and
  explicit model request settings without silently changing their meaning. Judges
  share model fields; preserve independent evaluation defaults and validation.
- Match model IDs to the served catalog. Keep credential values out of profiles,
  metadata, logs, and results; use configured environment-variable references.
  For no-cost model/tool smoke runs, use the [mock LLM guide](../docs/implementation/mock-llm.md).
- Preserve [DeepSeek credential-file validation and fallback](../docs/configuration.md#external-deepseek).
- For managed SGLang, validate endpoint and measurement settings using full option
  names; reject aliases and abbreviations that bypass those checks, keys containing
  `=`, and scalar values or list items starting with `-` unless numeric. Pass other
  native options through for SGLang to validate.
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
