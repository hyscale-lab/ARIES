# Sandbox-call replay limitations

## Overview

This document describes the implementation at `44f5ee2`. It separates current behavior from [proposed fail-fast policy](#8-proposed-fail-fast-policy-not-implemented).

Replay replaces the harness and model with a caller. The caller sends recorded sandbox requests through a new private bridge. Each occurrence has a fresh prepared sandbox. The [supported workload](#1-workload-and-profile-boundaries) includes multiple tasks, repeated tasks, finite recorded loops, and empty call logs. The one-task Runner guard is not a whole-run limit.

The loader checks all selected [source evidence](#2-source-evidence-and-compatibility) before task sandbox creation. [Transport and request rules](#3-transport-and-request-boundaries) and [size bounds](#4-size-and-capture-bounds) can reject a source. The caller preserves a recorded start schedule, not exact [physical timing or resource contention](#5-timing-and-execution-fidelity). Strict response checks differ from [diagnostic drift and reused grades](#6-response-checks-and-result-meaning). [Capture and cleanup failures](#7-current-errors-and-cleanup-safeguards) have different effects.

Replay can repeat supported bridge-visible behavior. It does not prove the same final sandbox state. An error is appropriate when required evidence is missing. An error cannot detect a state difference that the trace never observed. No representative workload evidence here establishes that all remaining limits are minor. The [design](design/replayer.md) gives the full component flow.

## 1. Workload and profile boundaries

### Supported workload, not a single-task restriction

Recording supports multiple logical tasks, repeated task IDs, selector expansion, and finite timed loops. Replay executes each selected recorded occurrence once. Empty call logs are valid when their metadata and source evaluation are complete. Tests cover distinct, repeated, loop-produced, and zero-call sources (`internal/app/run.go:164-175,357-395`; `internal/app/run_test.go:456-530,2226-2249`).

The following guard applies to one Runner. The application creates a fresh Runner for each admitted occurrence. It does not restrict the whole replay run to one task (`pkg/runner/runner.go:181-185`; `internal/app/run.go:379-395,605-613`).

```go
	const replayTaskCount = 1
	if r.caller != nil && len(tasks) != replayTaskCount {
		result.Duration = time.Since(started)
		return result, errors.New("replay requires exactly one task")
	}
```

`replay.source` selects one occurrence directory or all occurrences indexed by one parent `run-result.json`. One invalid indexed occurrence rejects a whole-run selection. There is no arbitrary subset selector or merged-run source. `benchmark.tasks` supplies the matching logical-task catalog; it does not filter or multiply recorded occurrences (`pkg/bridge/grpc/replay_sources.go:98-178`; `internal/app/run.go:357-395`).

Replay rejects `execution.loop_duration`, `execution.arrivals_file`, and `execution.arrival_rate_per_min`, even with explicit zero or empty values. This does not reject occurrences produced by a source loop. Replay uses that finite recorded set without creating new iterations (`pkg/config/config.go:715-725`; `internal/app/run.go:379-395`).

Recording requires the explicit `--record-for-replay` intent and `hermes-grpc`. Setup and replay reject this flag. Recording rejects arrival schedules. A recording loop deadline stops admission, not admitted work. Total recording duration can exceed that deadline (`internal/app/run.go:69-72,115-125`; `internal/app/execution.go:81-95,156-172`; `internal/app/run_test.go:2361-2363`).

A replay profile requires `replay.deployment` and omits `model`, `harness`, and model `runtime`. Benchmark setup, catalog loading, bridge preparation, and image preparation still run. Setup prepares dependencies but does not validate the replay source; source preflight occurs in the run path (`pkg/config/config.go:686-713`; `internal/app/run.go:80-85,313-333,420-476`).

**Sources:** `internal/app/run.go`, `internal/app/execution.go`, `internal/app/run_test.go`, `pkg/runner/runner.go`, `pkg/bridge/grpc/replay_sources.go`, `pkg/config/config.go`.

## 2. Source evidence and compatibility

Source validation is a safeguard, not a limit to remove. The loader requires real private directories with mode `0700` and regular evidence files with mode `0600`. It checks opened-file identity and bounded size. It requires schema version `1`, readiness, finalized log length and SHA-256, a complete final line, a completion marker, and task/runtime identity. Benchmark type, logical task, image reference, and benchmark revisions must match (`pkg/bridge/grpc/replay.go:61-124,223-266,278-281`).

```go
	if int64(len(log)) != *metadata.ToolCallsBytes || hex.EncodeToString(digest[:]) != metadata.ToolCallsSHA256 {
		return plan, errors.New("replay call log differs from finalized evidence")
	}
```

Every record must have continuous sequence identity, valid timestamps and response evidence, and a supported request. Rejected or canceled calls, any `replay_error`, and failed `WriteFile` calls reject the source. Complete admitted non-write failures and nonzero `Exec` exits can be valid expected outcomes. A recorded error is not automatically a replay execution error (`pkg/bridge/grpc/replay.go:278-363`; [response checks](#6-response-checks-and-result-meaning)).

The parent result must contain exactly one matching evaluation, explicit numeric score and reward, and status `succeeded`, `failed`, or `canceled`. The source need not have succeeded. Its evaluation is historical evidence, not a new grade (`pkg/bridge/grpc/replay.go:135-155`; [result meaning](#6-response-checks-and-result-meaning)).

Older finalized sources with calls may omit metadata `run_id` only when every call proves the parent binding. Empty sources require that metadata binding. Sources from before finalized capture evidence are invalid. Older `Exec` records without captured output can still pass when request and response checks are complete. Missing output does not automatically make hash comparison unavailable (`pkg/bridge/grpc/replay.go:99,132-134,278-281,321-330`; `pkg/replay/responses.go:128-163`).

Whole-run preflight retains compact identities, timing, and evidence bindings, not all request plans. Admission reloads one bounded plan and rejects changed metadata, call log, or parent result. A source change after initial preflight can therefore fail at admission. Initial preflight still precedes all task sandboxes (`pkg/bridge/grpc/replay_sources.go:20-54,159-175,236-259`; `internal/app/run.go:329-333,383-384`; `pkg/bridge/grpc/replay.go:217-218`).

A successful recording or `replay_ready: true` is not full parser approval. Finalization and parser eligibility are different checks. Some capture loss becomes an error only when the source is loaded ([current errors](#7-current-errors-and-cleanup-safeguards); `pkg/bridge/session.go:309-326`; `docs/run-results.md:280-285`).

**Sources:** `pkg/bridge/grpc/replay.go`, `pkg/bridge/grpc/replay_sources.go`, `pkg/replay/responses.go`, `internal/app/run.go`, `pkg/bridge/session.go`, `docs/run-results.md`.

## 3. Transport and request boundaries

Replay supports the `hermes-grpc` bridge and benchmark selectors `terminalbench2`, `deepresearchbench`, `sweatlasqa`, and `swebenchpro`. SSH and Toolathlon replay are unsupported. The `gRPC` bridge does not support Apptainer (`cmd/aries/wiring.go:58-82`).

Within a replay run, caller, bridge, and sandbox use the same supported backend. Docker placements use the same local Unix socket. Kubernetes placements use one namespace. A source recorded with Docker can replay with Kubernetes; the source loader does not require the old backend. That permission does not promise equal responses (`pkg/config/deployment.go:92-108,148-158,223-240`; `pkg/replay/managed/caller.go:77-87`; `pkg/bridge/grpc/replay.go:99-103`; [fidelity](#5-timing-and-execution-fidelity)).

The caller uses a private task attachment. The bridge uses plaintext and has no caller authentication. This is a trusted private-network contract, not a public replay service (`pkg/bridge/grpc/bridge.go:1-3`; `pkg/replay/managed/caller.go:77-98`).

The parser and caller support exactly five operations: `Exec`, `Stat`, `ReadFile`, `ReadLines`, and `WriteFile`. `Exec` requires canonical `/bin/bash -c` or `/bin/bash -l -c`, the recorded command hash, and a shell-neutral workdir. Replay does not remap the workdir; the selected or resolved endpoint workdir must match (`pkg/bridge/grpc/replay.go:306-363`; `pkg/replay/plan.go:63-70`; `pkg/runner/runner.go:565-570`).

```go
		login := slices.Equal(record.Argv, []string{bridgekit.RemoteShellPath, "-l", "-c", record.Command})
		if record.Path != bridgekit.RemoteShellPath || !login && !slices.Equal(record.Argv, []string{bridgekit.RemoteShellPath, "-c", record.Command}) {
			return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay Exec argv differs from the recorded command")
		}
```

Retained `Exec` stdin and `WriteFile` bodies must be valid UTF-8. They cannot contain control characters except tab, line feed, and carriage return. Binary request bodies are unsupported sources even when an optional raw log retained them. The loader does not use raw logs as a fallback (`pkg/bridge/internal/bridgekit/audit.go:366-375`; `pkg/bridge/grpc/audit.go:227-239`; `pkg/bridge/grpc/replay.go:91-120,311-319,351-359`).

This input rule does not forbid binary `Exec` output. Capture stores invalid UTF-8 output as base64. Read output is hashed, not retained as a response body. An optional `tool_call_id` has a 128-byte bound and an ASCII character set (`pkg/bridge/grpc/audit.go:99-110`; `pkg/bridge/grpc/files.go:450-473`; `pkg/bridge/grpc/bridge.go:67-72`; `pkg/bridge/grpc/replay.go:298-299`).

**Sources:** `cmd/aries/wiring.go`, `pkg/config/deployment.go`, `pkg/replay/managed/caller.go`, `pkg/bridge/grpc/bridge.go`, `pkg/bridge/grpc/replay.go`, `pkg/replay/plan.go`, `pkg/runner/runner.go`, `pkg/bridge/internal/bridgekit/audit.go`, `pkg/bridge/grpc/audit.go`, `pkg/bridge/grpc/files.go`.

## 4. Size and capture bounds

The bounds below have different scopes. A per-occurrence audit bound is not a whole-run task limit. There is no fixed call-count limit in the parser. The bounded parent result creates a practical occurrence ceiling, not a one-task restriction (`pkg/bridge/grpc/replay.go:158-184`; `pkg/bridge/grpc/replay_sources.go:68-78,121-175`).

| Boundary | Current bound and effect | Evidence |
| --- | --- | --- |
| Request capture | 16 MiB per `Exec` stdin or `WriteFile` body. Oversized stdin is refused before execution. An oversized write can still execute while capture drops its body. | `pkg/bridge/internal/bridgekit/audit.go:22-25`; `pkg/bridge/grpc/bridge.go:390-394`; `pkg/bridge/grpc/files.go:168-184` |
| `Exec` output | 16 MiB for each stdout and stderr stream. Docker and Kubernetes return an execution error when their output limit is exceeded. The bridge also has bounded output buffers. | `pkg/bridge/grpc/bridge.go:37-60,402-448`; `pkg/deployment/docker/exec.go:67-69,137-143`; `pkg/deployment/kubernetes/exec.go:57-59,92-97,213-220` |
| `gRPC` messages | 64 MiB per message. Generated read-response and replay-write chunks carry at most 64 KiB. Incoming `WriteFile` chunks must fit the declared remaining bytes and message limit, not a 64 KiB cap. This is not a total read-file limit. | `pkg/bridge/grpc/bridge.go:55-60,226-227`; `pkg/bridge/grpc/files.go:39-40,247-301,435-441`; `pkg/bridge/grpc/caller.go:209-212` |
| Source record | 128 MiB per JSONL line. Capture budgeting includes metadata, JSON escapes, base64, and later sequence/time stamps. | `pkg/bridge/grpc/replay.go:159-170`; `pkg/bridge/grpc/audit.go:113-150` |
| Audit | 256 MiB combined structured and optional raw audit per occurrence. Raw logging consumes the same budget. Overflow latches an error and prevents successful finalization. | `pkg/bridge/internal/bridgekit/audit.go:22-25,130-165,250-258,336-350` |
| Collected artifact and source log | 256 MiB per artifact and source call log. Manifest, length, and digest checks still apply. | `pkg/bridge/artifacts.go:18,79-111`; `pkg/bridge/grpc/replay.go:25-27,114-120` |
| Source metadata and parent result | 64 MiB for each `runtime.json` and `run-result.json`. | `pkg/bridge/grpc/replay.go:91`; `pkg/bridge/grpc/replay_sources.go:68-78`; `pkg/bridge/grpc/bridge.go:60` |
| Encoded caller plan | 512 MiB per occurrence, including JSON/base64 expansion. Whole-run preflight reserves shared-origin and global-offset overhead. | `pkg/replay/plan.go:50-60`; `pkg/bridge/grpc/replay_sources.go:184-213` |
| Managed caller files | 1 MiB for launch/status files; the plan has its separate bound. Private-file checks apply. | `pkg/replay/managed/caller.go:25-35,234-255`; `pkg/replay/managed/child.go:25,74,151-168` |

The audit budget is charged after encoding both records. It does not silently retain a partial record (`pkg/bridge/internal/bridgekit/audit.go:143-163`).

```go
	charge := int64(len(structuredLine) + len(rawLine))
	if charge > maxToolLogBytes-writer.bytes {
		writer.latchLocked(fmt.Errorf("combined audit exceeds %d bytes", maxToolLogBytes))
		return
	}
```

`ReadFile` with zero `max_bytes` reads to the end. `ReadLines` has a requested window and optional line clamp. Neither the 64 KiB chunk size nor the 16 MiB request-capture bound is a total read-response cap (`pkg/bridge/grpc/files.go:247-301,339-379`).

Response fingerprints cover the canonical outcome sent by the bridge, not bytes discarded before that response. They are not proof of all output produced in the sandbox. Encoded output capture can also overflow its record budget without changing the live RPC result ([current errors](#7-current-errors-and-cleanup-safeguards); `pkg/bridge/grpc/bridge.go:425-448`; `pkg/bridge/grpc/audit.go:137-150`).

**Sources:** `pkg/bridge/internal/bridgekit/audit.go`, `pkg/bridge/grpc/bridge.go`, `pkg/bridge/grpc/files.go`, `pkg/bridge/grpc/caller.go`, `pkg/bridge/grpc/audit.go`, `pkg/bridge/grpc/replay.go`, `pkg/bridge/grpc/replay_sources.go`, `pkg/bridge/artifacts.go`, `pkg/replay/plan.go`, `pkg/replay/managed/caller.go`, `pkg/replay/managed/child.go`, `pkg/deployment/docker/exec.go`, `pkg/deployment/kubernetes/exec.go`.

## 5. Timing and execution fidelity

Replay schedules concurrent RPC launches from recorded `started_at`, not file order or source completion order. The earliest selected source call is the common source origin; replay does not reproduce the delay before it. Equal starts can launch in any order. The application and Runner warn about equal timestamps. The caller starts overdue calls without resetting later targets. It does not serialize calls to reproduce source completion dependencies (`pkg/bridge/grpc/replay.go:190-203`; `pkg/bridge/grpc/replay_sources.go:168-172,195-213,215-220`; `pkg/bridge/grpc/caller.go:52-96`; `pkg/runner/runner.go:529-535`).

```go
			wait := time.Until(origin.Add(call.StartOffset))
			if wait <= 0 {
				break
			}
```

For multiple selected occurrences, the first ready caller with calls arms the shared UTC origin when `Run` begins. Empty occurrences do not arm it. There is no all-callers readiness barrier. A single occurrence keeps a caller-local origin. Late preparation, staging, and network delivery can delay actual starts; later callers keep the same shared targets (`internal/app/run.go:345-373`; `pkg/replay/managed/caller.go:146-172`; `pkg/bridge/grpc/caller.go:52-96`).

The schedule assumes stable, synchronized source clocks. A shared replay origin also assumes synchronized application and caller clocks; shared timing diagnostics need aligned bridge clocks. Audit timestamps discard monotonic clock data. Exact physical starts, equal-start order, overlap duration, and completion order are not guaranteed (`pkg/bridge/internal/bridgekit/audit.go:57-59`; `pkg/bridge/grpc/caller.go:52-96`; `docs/design/replayer.md:131-135`).

`execution.concurrency` limits occurrence ownership through preparation, execution, observation, and cleanup. It is not an in-flight RPC limit. Each caller launches due calls concurrently without a separate RPC concurrency setting. Original sandbox counts, lifecycle timing, and resource contention are not reproduced (`internal/app/execution.go:87-133`; `internal/app/run.go:390-415`; `pkg/bridge/grpc/caller.go:57-106`; `docs/design/replayer.md:183`).

Replay starts and prepares a fresh sandbox. It does not restore a snapshot. Matching task metadata cannot prove the same initial bytes: tagged images can change. The image rule requires tags and rejects digest-bearing references (`pkg/runner/runner.go:543-556`; `pkg/bridge/grpc/replay.go:102-103`; `pkg/containerimage/image.go:20-38`).

Each RPC receives the current task timeout or override, not a recorded per-call deadline. Managed execution is one-shot. Readiness, staging, and execution have bounded budgets; invalid or overflowing durations fail. There is no application-level call retry, adaptive plan change, recorded-response injection, speed control, pause, or step mode. Configured `gRPC` retries are disabled. Transparent retries can still occur before a wire write or before server processing (`internal/app/run.go:592-596`; `pkg/bridge/grpc/caller.go:91-96`; `pkg/replay/managed/caller.go:123-148,153-173`; `pkg/replay/managed/child.go:39-41,91-113`; `pkg/config/config.go:107-110`; dependency `google.golang.org/grpc@v1.71.1/dialoptions.go:641-645`).

For traces with calls, the caller waits the recorded final interval after all launched calls finish. Slow responses can extend total duration. Empty traces add no artificial wait. Completion markers describe bridge receipt after confirmed harness stop, not exact harness lifetime (`pkg/bridge/grpc/replay.go:205-209`; `pkg/bridge/grpc/caller.go:19-21,98-120`; `docs/design/replayer.md:115,135`).

**Sources:** `pkg/bridge/grpc/replay.go`, `pkg/bridge/grpc/caller.go`, `pkg/bridge/grpc/replay_sources.go`, `pkg/runner/runner.go`, `internal/app/run.go`, `internal/app/execution.go`, `pkg/replay/managed/caller.go`, `pkg/replay/managed/child.go`, `pkg/bridge/internal/bridgekit/audit.go`, `pkg/containerimage/image.go`, `pkg/config/config.go`, `docs/design/replayer.md`; dependency `google.golang.org/grpc@v1.71.1/dialoptions.go`.

## 6. Response checks and result meaning

The caller strictly checks canonical response size and `gRPC` status for every call. It also checks `Exec` exit code and successful `WriteFile` byte count. Canonical size includes the method, encoded messages, and final status/message; it is not only output length (`pkg/bridge/grpc/caller.go:222-227`; `pkg/bridge/grpc/audit.go:57-94`).

```go
	if outcome.invalid || outcome.bytes != call.ResponseBytes || status.Code(err).String() != call.ResponseCode || exitCodeChanged || written != nil && written.GetBytesWritten() != int64(len(call.Body)) {
		return fmt.Errorf("replay call %d %s response mismatch", call.Sequence, call.Operation)
	}
```

A strict mismatch already stops later calls in that occurrence. The caller cancels active call contexts and waits for completion or bounded termination handling. This is not a whole-run stop rule. Other occurrences can continue when cleanup is confirmed (`pkg/bridge/grpc/caller.go:39-45,74-109`; `internal/app/execution.go:125-133,156-172`).

A SHA-256 difference alone is nonfatal. Equal-length content changes, changed response fields, or equal-length error-message changes can therefore remain diagnostic when strict checks still match. Expected recorded non-OK outcomes can pass. There is no strict-hash profile switch (`pkg/bridge/grpc/caller.go:222-227`; `pkg/replay/responses.go:128-163`; `pkg/config/config.go:107-110`).

After confirmed isolation, Runner collects response-hash and timing diagnostics. Missing, duplicate, ambiguous, or unrepresentable evidence can make a summary `unavailable` without failing replay. Missing captured `Exec` output alone does not prevent hash correlation. Local timing uses the first observed call as its origin; shared timing keeps displacement from the common replay origin (`pkg/runner/runner.go:609-629`; `pkg/replay/responses.go:99-169`; `pkg/replay/timing.go:21-26,64-106`).

Replay does not run the harness, model, verifier, or judge. It copies the source evaluation and marks it reused. Reused evaluations do not count as new evaluations. The run summary has no dedicated replay or drift counters (`pkg/runner/runner.go:538-541,543-631,727-737`; `pkg/core/types.go:307-315`).

Matching bridge responses do not prove the same final filesystem or process state. A command can write a time-dependent file without returning its content. Background work can continue after an RPC. These differences need not cause any response mismatch. Reused grades cannot validate the new sandbox or hide a replay execution or isolation failure (`docs/design/replayer.md:175,187,195-201`; `docs/design/bridge.md:167-176`).

**Sources:** `pkg/bridge/grpc/caller.go`, `pkg/bridge/grpc/audit.go`, `pkg/replay/responses.go`, `pkg/replay/timing.go`, `pkg/runner/runner.go`, `internal/app/execution.go`, `pkg/config/config.go`, `pkg/core/types.go`, `docs/design/replayer.md`, `docs/design/bridge.md`.

## 7. Current errors and cleanup safeguards

Current behavior differs by boundary. Unsupported source evidence fails preflight before a task sandbox starts. Runtime mismatches and readiness failures can be known only during execution. Recording finalization does not run every source-parser check ([source validation](#2-source-evidence-and-compatibility); `internal/app/run.go:313-333`; `pkg/runner/runner.go:543-585`; `pkg/bridge/session.go:309-326`).

A recording `Exec` response that exceeds the encoded debug budget loses its captured output and gets `response-debug-too-large`. The fingerprint and live RPC result remain. Bridge stop can succeed, but readiness becomes false. After confirmed isolation, Runner fails the recording task and skips evaluation. This error arrives after the call, not before its side effects (`pkg/bridge/grpc/audit.go:137-150`; `pkg/bridge/session.go:309-317`; `pkg/runner/runner.go:406-417`; `pkg/replay/responses.go:58-69`).

```go
	record.ResponseDebug = false
	record.ResponseStdout, record.ResponseStderr = "", ""
	record.ResponseStdoutBase64, record.ResponseStderrBase64 = nil, nil
	record.ReplayError = "response-debug-too-large"
```

The same marker alone does not fail the current replay execution. The replay Runner does not apply the live audit check; response correlation reads hashes without rejecting `replay_error`. Capture loss can therefore leave a successful replay with reduced output evidence (`pkg/runner/runner.go:578-629`; `pkg/replay/responses.go:99-108,128-163`).

Omitted binary input, oversized write bodies, failed writes, and other parser-ineligible calls can leave a recording that only fails at source loading. `LiveAuditError` specifically detects the encoded `Exec` output overflow; it is not full replay validation. This is a current fail-fast gap, not a claim that those sources are supported (`pkg/bridge/grpc/bridge.go:439-440`; `pkg/bridge/grpc/files.go:168-184,476-482`; `pkg/replay/responses.go:46-69`; `pkg/bridge/grpc/replay.go:278-281,311-319,351-359`).

Combined audit overflow is different. It latches a writer error, later calls must be refused, and finalization fails. This applies to ordinary live, recording, and replay sessions. Ordinary live runs do not enable replay response capture or the recording-only Runner audit check (`pkg/bridge/internal/bridgekit/audit.go:130-165,250-258,336-350`; `pkg/bridge/grpc/audit.go:50-55`; `pkg/runner/runner.go:407-417`).

Positive caller stop, bridge revocation, and sandbox removal remain required. Unconfirmed cleanup stops new occurrence admission before the failed slot is released. Admitted peers drain; retained owners receive bounded cleanup retries. Recovery does not erase the original error and does not guarantee eventual removal. A transport-close error after confirmed runtime stop remains an error, but is not live-owner uncertainty (`internal/app/execution.go:125-133`; `internal/app/run.go:390-415,621-650`; `pkg/runner/runner.go:586-629`; `pkg/replay/managed/caller.go:197-216`).

Call cancellation or timeout does not prove termination of background processes. Native Docker and Kubernetes execution can leave commands running until exit or sandbox removal. Fail-fast policy must not replace positive cleanup with an assumption that cancellation removed all work (`docs/design/bridge.md:167-176`; `pkg/deployment/docker/exec.go:60-61`; `pkg/deployment/kubernetes/exec.go:50-51`).

**Sources:** `internal/app/run.go`, `internal/app/execution.go`, `pkg/runner/runner.go`, `pkg/bridge/session.go`, `pkg/bridge/grpc/audit.go`, `pkg/bridge/grpc/bridge.go`, `pkg/bridge/grpc/files.go`, `pkg/bridge/grpc/replay.go`, `pkg/replay/responses.go`, `pkg/bridge/internal/bridgekit/audit.go`, `pkg/replay/managed/caller.go`, `pkg/deployment/docker/exec.go`, `pkg/deployment/kubernetes/exec.go`, `docs/design/bridge.md`.

## 8. Proposed fail-fast policy (not implemented)

**Proposed policy. This section does not change the current contract.**

Reject an unrepresentable recording at the earliest safe boundary. Reject known unsupported request input before dispatch when possible. Report output-capture loss when it becomes known. Preserve the actual RPC result and any side effects already made. Always run positive cleanup. These recommendations address the [current gaps](#7-current-errors-and-cleanup-safeguards), rather than remove evidence or isolation safeguards.

Do not treat an expected recorded non-OK outcome as an unexpected failure. Compare it with the recorded expectation. The current caller already supports this distinction and stops on strict mismatches (`pkg/bridge/grpc/caller.go:222-227,39-45`; [response rules](#6-response-checks-and-result-meaning)).

The following stronger rules need an explicit contract decision: fatal SHA-only drift, fatal diagnostic unavailability, a timing-drift threshold, or stopping future occurrences on the first response mismatch. Stopping future calls on SHA-only drift is also a change; stopping future calls on strict mismatch already exists. No strict-hash or timing-policy switch currently exists (`pkg/config/config.go:107-110`; `pkg/bridge/grpc/caller.go:39-45,222-227`; `pkg/runner/runner.go:609-629`; `internal/app/execution.go:125-133`).

Any early-stop rule can stop execution before the finite source workload is complete. Calls already started cannot be undone. Concurrent occurrences may already own live runtimes. A stricter policy must retain those facts in results and continue bounded cleanup ([execution](#5-timing-and-execution-fidelity), [cleanup](#7-current-errors-and-cleanup-safeguards)).

An error is the appropriate outcome for missing required capture. It is not a substitute for defining replay fidelity. Timing variation, unobserved state, and reused evaluation are separate limits. They can occur without overflow or malformed input ([timing](#5-timing-and-execution-fidelity), [result meaning](#6-response-checks-and-result-meaning)).

**Sources:** `pkg/config/config.go`, `pkg/bridge/grpc/caller.go`, `pkg/runner/runner.go`, `internal/app/execution.go`. Recommendations above are policy proposals, not implementation claims.
