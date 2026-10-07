# Validation

- Choose checks for the changed behavior; reuse coverage and avoid tests of trivial
  forwarding. The full suite is for release validation, not every edit.
- Release checks: `make build`, `make test`, `make test-race`, `make lint`,
  `make integration`. Unit/race tests require neither Docker nor paid APIs;
  integration uses real containers and a deterministic fake model endpoint.
- Make configures local Go caches; direct Go commands may need
  `GOCACHE=$PWD/.cache/go-build GOMODCACHE=$PWD/.cache/go-mod`.
- Documentation-only edits need content/link/anchor checks and `git diff --check`.
- Paid runs require authorization. Use the existing
  `profiles/{hermes,openclaw}-tb2-fix-git-deepseek.json` smoke profiles when relevant.
  Inspect `runs/<run-id>/run-result.json`: harness outcome, evaluation score,
  confirmed isolation, cleanup, and each task's `error`. Exit status alone is
  insufficient.
- After runtime work, check for leaked containers, networks, processes, listeners,
  and credential files. Scan private artifacts without printing secrets.
- Keep credentials, datasets, and run artifacts ignored. Report actual evidence
  and validation gaps.

Details: [Makefile](../Makefile), [run/artifact guide](../docs/run-results.md).
