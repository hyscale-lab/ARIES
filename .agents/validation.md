# Validation

- Choose checks for the changed behavior; reuse coverage and avoid trivial
  forwarding tests. Assert observable contract behavior, not incidental wrappers
  or structure. Existing tests alone do not justify an extra policy. The full
  suite is for release validation, not every edit.
- Release checks: `make build`, `make test`, `make test-race`, `make lint`,
  `make integration`. Unit/race tests require neither Docker nor paid APIs;
  integration uses real containers and a deterministic fake model endpoint.
- Make configures local Go caches; direct Go commands may need
  `GOCACHE=$PWD/.cache/go-build GOMODCACHE=$PWD/.cache/go-mod`.
- Documentation-only edits need content/link/anchor checks and `git diff --check`.
- Paid runs require authorization. Use the existing
  `profiles/{hermes,openclaw}-tb2-fix-git-deepseek.json` smoke profiles when relevant.
  Inspect `runs/<run-id>/run-result.json`: harness outcome, evaluation score,
  confirmed isolation, task cleanup/errors, and shared infrastructure observation
  and cleanup/errors. Exit status alone is insufficient.
- For memory tracing, follow the selected [backend protocol and source pins](../docs/experiments/memory-footprint-tracing.md);
  validate native boundaries, acknowledgement identities and cancellation
  separately from page coverage. Reject contradictory compact partitions, unions
  or counts even when source hashes match. Keep native bit work, host blocking,
  decoder detail and publication costs distinct; never add overlapping spans or
  infer native overhead from an in-memory fixture. Preserve the declared workload
  inventory, incomplete windows and unknowns;
  fixture success does not prove completed benchmark collection.
- After runtime work, check for leaked containers, networks, processes, listeners,
  and credential files. Scan private artifacts without printing secrets.
- Keep credentials, datasets, and run artifacts ignored. Report actual evidence
  and validation gaps. Keep latency observations tied to their recording boundary;
  preserve unknown timings and overlapping stages. For page observations, keep
  session/turn/step identity and native step boundaries; one step may contain
  multiple sandbox operations. Unknown pages are not idle or zero. See
  [command latency](../docs/run-results.md#command-latency).

Details: [Makefile](../Makefile), [run/artifact guide](../docs/run-results.md).
