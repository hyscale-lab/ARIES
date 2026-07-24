# ARIES Design

The maintained architecture document is [docs/design.md](docs/design.md).

The Runner still composes exactly four substitutable roles. For each task it
loads benchmark data, starts the sandbox, asks the benchmark to sanitize the
live sandbox, starts the bridge and harness, positively stops the harness and
revokes the bridge, evaluates the still-running sandbox, then stops the
sandbox. Terminal-Bench verifier files remain benchmark-private and are
uploaded only from the freshly reverified pinned checkout after both isolation
gates succeed.

Profiles may reference one dedicated strict-JSON runtime override file.
Omitted resources preserve benchmark sandbox values and leave the matching
harness dimension unlimited; present CPU or memory applies to both containers,
while an agent timeout changes only its task deadline. Task containers receive
ARIES-owned timezone and noninteractive values. Each bridge retains structured
tool records plus sensitive exact-wire `bridge/ssh_raw.log` evidence through a
bounded asynchronous writer whose failure blocks positive bridge revocation.

## Repository boundary

- `aries` is the only write and Git-operation boundary.
- `invitro` was read only and served only as a structural reference.
- `agent_bench` was read only and served only as workflow archaeology.
- No source was copied from either reference repository.
- Before this boundary was recorded, one read-only `git status` ran in each
  reference repository. Neither repository was changed.

Generated datasets, run artifacts, credentials, and OMX state remain inside
`aries` and are excluded from Git.
