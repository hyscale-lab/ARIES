# Validation and run evidence

For code changes, begin with focused package tests. Add a regression for uncovered
changed behavior. State the intended boundary before refactors and preserve
existing behavior. Release checks: `make build`, `make test`, `make test-race`,
`make lint`, `make integration`. Make uses repository-local Go caches; direct Go
commands may need `GOCACHE=$PWD/.cache/go-build GOMODCACHE=$PWD/.cache/go-mod`.

Unit/race tests need no Docker or paid API. Integration tests use disposable
Docker containers and a fake endpoint; `make integration` builds the SSH helper.
Use authorized Docker access without relaxing ownership or credential checks.
Documentation-only changes need content, link, anchor, and consistency checks
including new files, plus `git diff --check`.

Live runs require user authorization. Existing Flash smoke profiles:

- `profiles/hermes-tb2-fix-git-deepseek.json`
- `profiles/openclaw-tb2-fix-git-deepseek.json`

Build, run the requested profile, then inspect its private `runs/<run-id>/`
artifacts. Check `run-result.json` for harness status, independent evaluation
score, confirmed harness stop/bridge revocation, and cleanup. Use task logs to
diagnose failures; exit status alone is insufficient.

After runtime work, verify no owned containers, networks, processes, listeners,
or temporary credential files remain. Keep original credentials private; scan
artifacts without printing secrets. Report validation gaps explicitly.

Human references: [run/artifact guide](../docs/run-results.md),
[build targets](../Makefile).

Before completion, review scope and preserve unrelated work. Keep datasets,
credentials, run artifacts, and OMX state ignored. Never print secrets or include
keys in profiles, metadata, logs, or results. Report actual checks and gaps;
follow the user's commit/staging instructions.
