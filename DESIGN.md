# ARIES Design

The maintained architecture document is [docs/design.md](docs/design.md).

## Repository boundary

- `aries` is the only write and Git-operation boundary.
- `invitro` was read only and served only as a structural reference.
- `agent_bench` was read only and served only as workflow archaeology.
- No source was copied from either reference repository.
- Before this boundary was recorded, one read-only `git status` ran in each
  reference repository. Neither repository was changed.

Generated datasets, run artifacts, credentials, and OMX state remain inside
`aries` and are excluded from Git.
