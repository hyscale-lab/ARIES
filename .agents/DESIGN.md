# ARIES Design

The maintained public architecture document is [docs/design.md](../docs/design.md).

Detailed implementation decisions and rejected tool-bridge designs are retained
in [BRIDGE-ALTERNATIVES.md](BRIDGE-ALTERNATIVES.md). These internal records
complement the stable public contract; they do not add another Runner role.

The Runner still composes exactly four substitutable roles. For each task it
loads benchmark data, starts the sandbox, asks the benchmark to sanitize the
live sandbox, starts the bridge and harness, positively stops the harness and
revokes the bridge, evaluates the still-running sandbox, then stops the
sandbox. Terminal-Bench verifier files remain benchmark-private and are
uploaded only from the freshly reverified pinned checkout after both isolation
gates succeed.

Every checked-in profile explicitly declares `overrides_file`; an empty string
disables overrides without opening a file. A referenced strict-JSON override
keeps `harness_resources` and `agent_sandbox_resources` independent: omitted
harness dimensions stay unlimited, omitted sandbox dimensions retain the
benchmark values, and neither block inherits from the other. An agent timeout
changes only the harness run deadline. Task containers receive ARIES-owned
timezone and noninteractive values.

The command layer may schedule fresh one-task Runner compositions concurrently.
Profile order, including duplicate weights, determines admission and result
order. Each occurrence has an exact global execution ID for directories,
labels, monitoring, and results. A configured loop duration bounds admissions,
not cleanup: admitted occurrences always drain through the unchanged lifecycle.
`runtime.config.gpu_indices` is the only explicit GPU selection input.
Backend-specific resolution supplies the same effective list to runtime
construction and NVIDIA sampling without changing the Runner or Recorder
lifecycle. SGLang is the current implementation of this rule.

Model selection is explicit through `runtime.backend`: `deepseek` retains its
official bounded preflight, while `sglang` performs bounded exact model
discovery at a versioned `/v1` endpoint. A local SGLang profile references one
strict native YAML file under `runtime.config.file`, whose
served model and port must match the profile. SGLang may remain external or run
as one application-owned host process supplied by an explicit command switch.
The general runtime lifecycle/health interface is separate from inference and
is not a fifth Runner role. Neither provider stores key bytes in profiles or
artifacts, and ARIES does not manage model installation.

Terminal-Bench 2 remains pinned by its exact Git revision, while each task's
explicit Docker image tag is read from that pinned task's `task.toml` rather
than repeated in `configs/versions.json`. OpenClaw uses an exact non-`latest`
release tag. Each task workdir is derived from its pinned Dockerfile with `/`
as the conservative fallback. The bridge maps OpenClaw's pinned virtual workspace to that workdir
without creating a sandbox symlink. It retains structured JSONL tool records
plus sensitive, lossless human-readable `bridge/ssh_raw.log` evidence through
a bounded asynchronous writer whose failure blocks positive bridge revocation.

## Repository boundary

- `aries` is the only write and Git-operation boundary.
- `invitro` was read only and served only as a structural reference.
- `agent_bench` was read only and served only as workflow archaeology.
- No source was copied from either reference repository.
- Before this boundary was recorded, one read-only `git status` ran in each
  reference repository. Neither repository was changed.

Generated datasets, run artifacts, credentials, and OMX state remain inside
`aries` and are excluded from Git.
