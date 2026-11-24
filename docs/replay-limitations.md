# Sandbox-call replay limitations

Use the [replay design](design/replayer.md) for the mechanism and
[configuration reference](configuration.md#sandbox-call-replay) for profile fields.
The limits below describe current behavior, not the
[proposed fail-fast policy](design/replayer.md#10-open-questions-and-deferred-features).

## Workload and source limits

- Recording requires `--record-for-replay` before the profile and `hermes-grpc`.
  It supports multiple tasks, repeated IDs, selector expansion, and finite timed
  loops, but not arrival schedules. A loop deadline stops admission, not admitted work.
- Replay selects one occurrence or every occurrence in one recorded run. It runs
  each once, including empty traces. There is no arbitrary subset or merged-run source.
  `benchmark.tasks` supplies the validation catalog, not an occurrence filter.
- Replay forbids new loop iterations, arrival settings, and live model, harness,
  or model-runtime fields. Setup prepares dependencies but does not validate sources.
- Only `hermes-grpc` on Docker or Kubernetes is supported, not SSH, Toolathlon,
  or Apptainer. Caller, bridge, and sandbox need the same backend and private
  attachment; Docker needs one local socket, Kubernetes one namespace.
  The unauthenticated bridge must stay on a trusted private network.
- Sources need private directories (`0700`), regular evidence files (`0600`),
  finalized complete logs, matching benchmark and logical task, and a unique
  parent occurrence. A source grade is optional. Preflight warns if the source
  image or benchmark revision differs from the target or is unverified.
  Incomplete, changed, rejected, canceled, or failed-write evidence rejects the source.
  Complete admitted non-write failures and nonzero `Exec` exits can be valid expectations.
- Old sources without finalized capture evidence are invalid. Older nonempty
  native sources can omit metadata `run_id` or `occurrence_id` only if every call
  proves the missing binding. Empty sources require both IDs in the metadata.
  Missing captured `Exec` output alone need not invalidate complete response checks.
- Only `Exec`, `Stat`, `ReadFile`, `ReadLines`, and `WriteFile` are supported.
  `Exec` requires the recorded canonical Bash argv and matching shell-neutral workdir.
  Binary or control-bearing request bodies are unsupported, even with raw logs.
  Binary `Exec` output can use base64; read-response bodies are hashed, not retained.

## Harbor conversion limits

- The Python converter supports the Codex artifact format for
  `terminal-bench/terminal-bench-2-1`. It needs each trial's `config.json`,
  `result.json`, `agent/codex.txt`, and one `agent/sessions/**/rollout-*.jsonl`.
  An optional `lock.json` and optional `agent/trajectory.json` must agree
  with the required evidence. A job input also needs a job `result.json`
  that accounts for every trial directory.
- The converter accepts matching Bash commands with recorded workdirs, and
  successful patches that add one file. For example, a wrapper that runs
  `printf ready` in `/tmp` and, if a session ID exists, waits with empty input
  becomes one `Exec` with recorded output `ready` and exit code `0`.
  The trace must establish the final result. There is no second poll call.
  Input sent to a process, termination or later reuse of a session, and
  commands built from earlier responses are unsupported. Patch updates and
  deletes are unsupported.
- Missing, ambiguous, or unsupported evidence stops conversion at the first
  failure. Check stderr for the trial, reason, and call when available.
  A complete trace with no tool calls is valid; a missing trace is not.
  The converter does not publish partial output or skip a bad trial.
- Output diagnostics cannot establish a mismatch when capture evidence is
  unavailable. Unknown stream interleaving permits a length check, not
  exact-byte comparison. Known truncation makes the output check inconclusive.
  An independently known exit mismatch still fails.

## Size and capture bounds

Bounds apply per occurrence unless stated otherwise. There is no fixed parser
call-count limit. The parent-result bound creates a practical occurrence limit.

| Boundary | Bound and effect |
| --- | --- |
| Request capture | 16 MiB per `Exec` stdin or write body. Oversized stdin fails before execution; oversized writes can execute without retained bodies. |
| `Exec` output | 16 MiB each for stdout and stderr. Docker and Kubernetes return an execution error above that bound. |
| gRPC message | 64 MiB per message. Read-response and replay-write chunks use at most 64 KiB; incoming writes need not use that chunk size. Neither is a total read-file limit. |
| Source record | 128 MiB per JSONL line, including JSON/base64 expansion, metadata, and stamps. |
| Audit | 256 MiB combined structured and optional raw audit. Overflow prevents finalization. |
| Collected artifact/source log | 256 MiB each, with manifest, length, and digest checks. |
| Source metadata/parent result | 64 MiB per file. |
| Encoded caller plan | 512 MiB per occurrence, including encoding and shared timing fields. |
| Managed caller launch/status | 1 MiB per file; the plan has its separate bound. |

A successful recording or `replay_ready: true` does not guarantee parser eligibility.
Omitted inputs, oversized writes, failed writes, and other incompatible calls can
fail only when loaded as a replay source. Recording checks do not cover every gap.
Encoded `Exec` capture overflow adds `response-debug-too-large`; after confirmed
isolation, the recording task fails without evaluation. This happens after the
call and its side effects. In replay, that marker alone can leave execution
successful with reduced output evidence. Combined audit overflow is different:
it blocks finalization and confirmed bridge revocation in all execution modes.
Keep audits, run logs, and stderr private; output can contain task data or credentials.

## Fidelity and result limits

- Replay needs stable, synchronized source clocks. Shared scheduling also needs
  synchronized app/caller clocks; timing diagnostics need an aligned bridge clock.
  Exact physical starts, equal-start order, overlap duration, and completion order
  are not guaranteed. Readiness, staging, and network delays can shift starts.
- `execution.concurrency` bounds occurrence owners, not in-flight calls. Replay
  does not reproduce sandbox lifecycle timing, original sandbox counts, or contention.
  It prepares fresh sandboxes, not snapshots; image tags can resolve to different bytes.
  Docker-to-Kubernetes replay is allowed, not guaranteed equivalent.
- Each RPC uses the current task timeout or override, not a recorded deadline.
  There is no application retry, adaptive plan, response injection, speed control,
  pause, or step mode. gRPC transparent retries can still occur before processing.
  Slow calls can extend duration beyond the source's final wait.
- Strict response size, gRPC status, `Exec` exit code, and successful write count
  differences fail an occurrence when the source supplies comparable evidence.
  Native SHA-only drift, Harbor same-length content drift, and timing drift remain nonfatal.
  Diagnostic evidence can be `unavailable` without failing replay.
- Source grades are historical, not new evaluations. No verifier or judge runs.
  Equal bridge responses do not prove equal final filesystem or process state.
  Reused grades cannot hide replay or isolation failure. Run summaries have no
  dedicated replay or drift counters.
- Unconfirmed cleanup stops new admission; admitted peers drain and bounded cleanup
  retries continue. Recovery does not erase the original error or guarantee removal.
  A strict response mismatch alone does not stop admission when cleanup is confirmed.
- Cancellation or timeout does not prove command or background-process termination.
  Caller stop, bridge revocation, and sandbox removal still need positive confirmation.
