# Validation

- Choose checks for the changed behavior; reuse coverage and avoid trivial
  forwarding tests. Assert observable contract behavior, not incidental wrappers
  or structure. Existing tests alone do not justify an extra policy. The full
  suite is for release validation, not every edit.
- Release checks: `make build`, `make test`, `make test-race`, `make lint`,
  `make integration`. Unit/race tests require neither Docker nor paid APIs;
  integration uses real containers and a deterministic fake model endpoint.
- When changing `pkg/deployment/apptainer`, also run `make integration-apptainer`
  on a host with Apptainer; see [its tests](../docs/implementation/apptainer.md#tests).
- Make configures local Go caches; direct Go commands may need
  `GOCACHE=$PWD/.cache/go-build GOMODCACHE=$PWD/.cache/go-mod`.
- Documentation-only edits need content/link/anchor checks and `git diff --check`.
- Paid runs require authorization. Use the existing
  `profiles/{hermes,openclaw}-tb2-fix-git-deepseek.json` smoke profiles when relevant.
  Inspect `runs/<run-id>/run-result.json`: harness outcome, evaluation score,
  confirmed isolation, task cleanup/errors, and shared infrastructure observation
  and cleanup/errors. Exit status alone is insufficient.
- After runtime work, check for leaked containers, networks, processes, listeners,
  and credential files. Scan private artifacts without printing secrets.
- With `--record-for-replay`, check the private Hermes gRPC audit before live
  evaluation. An oversized captured response fails its task after confirmed
  revocation. Replay automatically retains response and timing evidence. An
  aggregate audit failure does not confirm revocation; block live evaluation
  and report cleanup truthfully.
- Keep credentials, datasets, and run artifacts ignored. Report actual evidence
  and validation gaps.

Details: [Makefile](../Makefile), [run/artifact guide](../docs/run-results.md).
