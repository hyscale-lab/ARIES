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
  Incomplete or changed evidence rejects the source. Native replay also rejects
  refused or canceled calls and failed writes.
  Complete admitted non-write failures and nonzero `Exec` exits can be valid expectations.
  Strict preflight also requires historical comparison fields. Disabling
  comparison does not admit incomplete requests or unfinalized evidence.
- Old sources without finalized capture evidence are invalid. Older nonempty
  native sources can omit metadata `run_id` or `occurrence_id` only if every call
  proves the missing binding. Empty sources require both IDs in the metadata.
  Missing captured `Exec` output alone need not invalidate complete response checks.
- Only `Exec`, `Stat`, `ReadFile`, `ReadLines`, and `WriteFile` are supported.
  `Exec` requires the recorded canonical Bash argv and matching shell-neutral workdir.
  Binary or control-bearing request bodies are unsupported, even with raw logs.
  Binary `Exec` output can use base64; read-response bodies are hashed, not retained.

## Harbor conversion limits

- The Python converter supports `terminal-bench/terminal-bench-2-1`. Each
  trial needs `config.json` and `result.json` with matching identity. The
  agent name is part of that identity, not a format selector. An optional
  `lock.json` must agree with the trial package evidence. A job input also
  needs a job `result.json` that accounts for every trial directory.
- Codex-format evidence requires `agent/codex.txt` and one
  `agent/sessions/**/rollout-*.jsonl`. An optional `agent/trajectory.json`
  must agree with that evidence. A completed shell call can lack a numeric
  exit code. Conversion retains the observed output, but strict replay rejects
  the source.
- The converter accepts matching Bash commands with recorded workdirs, and
  successful patches that add one file. For example, a wrapper that runs
  `printf ready` in `/tmp` and, if a session ID exists, waits with empty input
  becomes one `Exec` with recorded output `ready` and exit code `0`.
  The trace must establish the final result. There is no second poll call.
  Input sent to a process, termination or later reuse of a session, and
  commands built from earlier responses are unsupported. Patch updates and
  deletes are unsupported.
- For a raw Claude Code Bash request without launcher evidence, the replay
  convention is `/bin/bash -c` with the recorded script unchanged. This is
  not evidence of the historical process arguments. Compatible recorded
  launcher arguments take precedence. Shell flags inside the script do not
  set the outer launcher mode.
- For Claude Code, one raw session supplies Bash requests and tool results
  with UTC times, call IDs, and the request workdir. `agent/claude-code.txt`
  must confirm the same session, tool events, and successful trace completion.
  Both artifacts must preserve call precedence. A request after another
  call's return cannot have an earlier timestamp. Equal boundaries and
  actual overlaps are valid.
  A raw session alone, an unmatched request, or a background start without
  correlated completion is unsupported. The converter preserves separately
  observed output and numeric exit code. For a Bash tool error, a leading
  `Exit code N` line and a matching `Error: ` wrapper establish the exit code.
  Diagnostic text after that line is not captured process output.
  A tool success flag is not a numeric exit code. Replay of
  partial response evidence requires `replay.must_check_response: false`.
- For Claude `Edit`, conversion preserves only the confirmed unread-file
  refusal. The raw result must link to the request and carry the specific
  refusal in both its error display and its result wrapper. The CLI transcript
  and any trajectory must confirm the event. A generic error or successful
  edit does not qualify. Replay retains this source event in both response
  modes, but does not apply the edit or recreate the tool handler's read state.
- The converter validates and omits historical Bash `timeout` and
  `dangerouslyDisableSandbox` values. These options do not change ARIES
  deadlines or task isolation. See the [Claude Bash examples](design/replayer.md#claude-bash-examples).
- When a Codex trace has a raw session, the raw session must agree with the
  transcript and rollout. For Bash, the session and call IDs, script, workdir,
  timestamps, output, and result status must match. The converter rejects
  extra, missing, and uncorrelated calls. It does not combine stdout and stderr
  when both contain data because their order is unknown.
  It retains the validated raw tool error flag independently of the exit code.
- A mixed trace can use literal script text in the transcript's `command`
  field instead of launcher arguments. This requires the same text in the
  matched raw Bash request and rollout request. The transcript must still
  record the numeric exit code and captured output. The rollout must still
  record the workdir, call times, and complete turns. An explicit rollout
  login setting overrides the default. If the transcript supplies launcher
  arguments, they must agree with any explicit rollout login setting.
- Missing, ambiguous, or unsupported evidence stops conversion at the first
  failure. Check stderr for the trial, reason, and call when available.
  A complete Codex trace with no tool calls is valid; a missing trace is not.
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
- With the default comparison policy, strict native response size, gRPC status,
  `Exec` exit code, and successful write count differences fail an occurrence.
  SHA-only and timing drift remain nonfatal. Diagnostic evidence can be
  `unavailable` without failing replay. Without comparison, RPC errors,
  incomplete `Exec` operations, and short writes still fail.
  See [Harbor response comparison](design/replayer.md#harbor-response-comparison)
  for converted sources.
- Source grades are historical, not new evaluations. No verifier or judge runs.
  Equal bridge responses do not prove equal final filesystem or process state.
  Reused grades cannot hide replay or isolation failure. Run summaries have no
  dedicated replay or drift counters.
- Unconfirmed cleanup stops new admission; admitted peers drain and bounded cleanup
  retries continue. Recovery does not erase the original error or guarantee removal.
  A strict response mismatch alone does not stop admission when cleanup is confirmed.
- Cancellation or timeout does not prove command or background-process termination.
  Caller stop, bridge revocation, and sandbox removal still need positive confirmation.
