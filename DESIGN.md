# ARIES Design

The maintained architecture document is [docs/design.md](docs/design.md).

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

Terminal-Bench 2 remains pinned by its exact Git revision, while each task's
explicit Docker image tag is read from that pinned task's `task.toml` rather
than repeated in `configs/versions.json`. OpenClaw remains digest-pinned. Each
task workdir is derived from its pinned Dockerfile with `/` as the conservative
fallback. The bridge maps OpenClaw's pinned virtual workspace to that workdir
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
