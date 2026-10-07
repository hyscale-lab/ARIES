# Validation

- Begin with focused tests. Reuse coverage; add regressions for uncovered changed
  behavior, not trivial forwarding. Preserve behavior during refactors.
- Release checks: `make build`, `make test`, `make test-race`, `make lint`,
  `make integration`. Unit/race tests require neither Docker nor paid APIs;
  integration uses real containers and a deterministic fake model endpoint.
- Make configures local Go caches; direct Go commands may need
  `GOCACHE=$PWD/.cache/go-build GOMODCACHE=$PWD/.cache/go-mod`.
- Documentation-only edits need content/link/anchor checks and `git diff --check`.
- Paid runs require authorization. Use the existing
  `profiles/{hermes,openclaw}-tb2-fix-git-deepseek.json` smoke profiles when relevant.
  Inspect `runs/<run-id>/run-result.json`: harness outcome, evaluation score,
  confirmed isolation, and cleanup. Exit status alone is insufficient.
- After runtime work, check for leaked containers, networks, processes, listeners,
  and credential files. Scan private artifacts without printing secrets.
- Preserve unrelated work and follow the user's staging/commit instructions.
  Keep credentials, datasets, run artifacts, and OMX state ignored. Report actual
  evidence and validation gaps; never infer success from intended behavior.

Details: [Makefile](../Makefile), [run/artifact guide](../docs/run-results.md).
