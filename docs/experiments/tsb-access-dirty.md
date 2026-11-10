# Access and dirty maps across Terminal-Bench LLM steps

The preserved current backend is VMM-owned tracing. See the
[source and setup guide](memory-footprint-tracing.md) for exact Firecracker and
OpenClaw revisions and repository-local companion/layout paths. The external
host-memory observer is retained for historical comparison. Private `runs/`
paths and earlier experiment decisions are provenance, not required source
inputs or authorization to launch another workload.

This opt-in study retains one shared tool epoch per complete assistant tool-request group, plus initial and subsequent LLM-wait epochs. The user-selected baseline pauses the guest during LLM processing and resumes before native tool preparation. This changes asynchronous guest progress, clocks and timeouts; it is an experimental policy, not transparent observation. Original studies and frozen sources remain separate.

## Lifecycle and clocks

Select scripts/tsb-access-dirty-plugin with the new observer mode. Startup synchronously establishes the initial paused epoch. Native messages, requested IDs, expanded operations and parallel scheduling remain unchanged. The last expected native result closes the tool epoch. The next declaration waits at the metadata barrier, closes the gap, starts the next tool epoch and resumes before acknowledging admission.

The pinned OpenClaw image exposes model_call_started/model_call_ended IDs, durations, outcomes and optional first-byte timing. Hooks retain metadata only, never prompts, arguments or result contents. Dispatch is asynchronous: callback timestamps are observations, not exact wire instants. Retain native duration separately and finalize telemetry after harness shutdown. Awaited before_agent_finalize releases a completed terminal gap; native source guards exclude retries, errors, incomplete answers, aborts and timeouts. Agent-end is observational: native context compaction can emit it before continuing the same session. Service teardown does not certify terminal completion. After confirmed harness-container removal, the host performs an independent bounded cancellation/release barrier; bridge revocation follows with a fresh component cleanup context. Verification is withheld unless both isolation stages succeed. Native validation must prove release precedes the evaluator's first sandbox operation.

The opted-in research plugin requires hooks.allowConversationAccess=true for terminal hooks. The renderer grants permission only to aries-step-pages; other model/tool/plugin configuration is preserved.

The observer binds a session/turn before initialization or cancellation can
affect that session. Every successful acknowledgement carries that identity.
The plugin also requires the declared native session, tool IDs/names, batch and
remaining-result set to agree before confirming admission or completion. Invalid
acknowledgements remain measurement failures and cannot produce a successful
terminal seal. These checks do not rewrite native messages or scheduling.
A failed harness start can already have initialized the observer, so rollback
retains the occurrence identity for the later confirmed-stop release barrier.

Keep host monotonic pause, dirty/access, census, discovery, barrier, publication and resume times distinct. The full paused LLM interval is policy time. Keep measured nonoverlapping critical-path spans separate from that policy interval; the overhead report does not reconstruct uninstrumented latency.

## Raw data

The worker-specific FC build must enable dirty tracking before boot and support the paused-only epoch API. Initial arm discards boot dirty state once; each later collection atomically rotates it. Access observed/idle/unknown masks remain separate from KVM dirty and conservative VMM writable-buffer candidates. A VMM bit can mark descriptor capacity before actual I/O, so it need not prove a completed write.

Primary modeled demand is observed access union unknown-as-touched union KVM/VMM candidates; preserve the old access-only sensitivity. Do not relabel raw access evidence. Host-idle missed reads remain possible; controlled read-only fixture validation is mandatory.

Each epoch phase manifest indexes region-relative little-endian bitmaps with hashes/populations. Resident masks and gzip uint64 host-PFN arrays preserve GPA aliases and distinguish absent pages from resident unresolved PFN zero. VMM smaps_rollup RSS/PSS is separate. Physical backing estimates are not proof of system-wide reclaim, exclusivity or safe eviction.

Compact readers validate content relationships as well as hashes: access states
are disjoint, the VMM format partitions the declared RAM regions including
absence, unknown-reason masks partition the unknown set, and combined dirty
equals the KVM/VMM union. Retained summary counts must agree with those masks.
Contradictory evidence is rejected before expanded page lists are accepted for
derived comparisons. A closed bitmap partition validates the representation; it does
not establish complete hardware access detection.

The pinned FC HTTP pause also suspends its device event loop. Dirty transactions check asynchronous block completion and reject unsupported writers/devices. This backend-specific guarantee requires native tests. Retain gap dirty maps and sequential-boundary skew; common epoch tokens do not establish simultaneous hardware samples.

## Detailed attribution

Only fix-git, prove-plus-comm, overfull-hbox, rstan-to-pystan and schemelike-metacircular-eval enable P2. Adaptive metadata discovery and encoding run during the held-guest LLM interval. A strict barrier precedes the next resume. Cancellation stops and joins live guest reads before resuming; failed captures remain invalid.

Attribute effective demand while preserving raw access and additional KVM/VMM provenance. Record every attempted metadata page read, including partial failure reads. Intersect that set with following-gap dirty candidates: overlap invalidates the tool-boundary-state claim. Empty overlap is qualified by covered-writer guarantees.

Observer metadata reads can contaminate host-idle gap access. Retain the observer-read mask and raw overlap, without subtracting overlap and claiming guest-only activity. Re-arm tool access after the metadata barrier.

The companion's `attribution_detail` selects `category`, `process` or `file`
(default). Category mode skips per-boundary process/VMA/PTE, owner, full free-list
and filename walks; it retains coarse page categories, inode identity checks,
buddy-head-only evidence and explicit unresolved cases. Process mode adds
process/owner associations without resolving paths; file mode adds the full
filename traversal. Bootstrap validation remains common and is measured
separately. These are different observation scopes, not interchangeable output
labels. Skipped association fields are unavailable rather than evidence of no
owner or no backing path. See [detail configuration and costs](memory-footprint-tracing.md#attribution-detail-and-cost).

## Preparation and gates

Use explicit fresh root, selection file, access-dirty config and attribution config in suite preparation. Run uses frozen inputs. The suite chooses the new plugin and applies P2 only to the five tasks. The optional agent_timeout_overrides map accepts catalog task IDs and positive integer seconds; omitted configuration retains the original RStan 5400-second default, while an explicit empty map keeps all native agent deadlines. Each selected override is written to the native runtime override file and hash-bound into the profile and manifest. Verifier timeouts and native CPU/RAM remain unchanged. Historical reruns used Scheme 9600 seconds and RStan 21600 seconds in distinct roots. Those were explicit deviations from the original study; new runs must choose and record their own overrides. Automatic paid retries are disabled.

Local deterministic fixtures exercise identity mismatches, acknowledgement consistency, cancellation, compact masks and lifecycle failures. Native known read-only/write/write-restore/block/network truth tables, reset continuity, metadata consistency, PFN aliases and cleanup require separate worker validation. For a new study, validate the selected native configuration before interpreting workload measurements, and preserve failed/censored/unsupported outcomes. Source recovery and deterministic tests do not launch or authorize paid workloads.


In the legacy external observer, metadata cancellation has a bounded cooperative grace (default 5 seconds) after
the 60-second barrier. If live reads persist, the entire observer exits with code
86 after saving a fatal receipt and partial read provenance. The controller reaps
that owned process, verifies the VMM PID, start time and task identity, then resumes
the guest so native lifecycle cleanup can proceed. Such an attempt is invalid and
stops further admissions. No reader is allowed to survive that resume.

For relocated evidence, pass the retained phase.json sibling directory as
compact.expand_reading(reading, artifact_directory=...). The original absolute
source path remains provenance and need not exist on the destination node.


Successful terminal coverage ends at the awaited before_agent_finalize hook.
The plugin allows up to 1000 ms for pending model diagnostic callbacks, requires
all recorded model starts to have ends, closed groups, no active calls and no
measurement errors, then writes and fsyncs a terminal seal before requesting
guest release. An invalid seal still releases for cleanup but marks the tail
incomplete. Retained telemetry is exported before container removal; this is a
sealed agent-run horizon, not a claim to collect every service shutdown event.
The native hook's terminal guards exclude retries, aborts, timeouts and incomplete
assistant answers in the pinned OpenClaw source. A later modifying hook can still
request revision, so the configured composed terminal-hook set must also be
bound for an irrevocable successful horizon; universal terminality is not claimed.

Provider events retain a process-lifetime model_occurrence_id in addition to
the unchanged native run_id/call_id. Reused completed native IDs after compaction
have separate occurrences. Each start freezes model_gap_after_batch_id (null
initially, otherwise the last completed tool batch), and its end copies it.
Multiple sequential model occurrences may occupy the same paused gap without
additional page resets. Active duplicates, unmatched ends, overlapping providers
and model starts with an unfinished tool batch remain explicit errors.
Compaction summarization that bypasses these provider hooks remains unclassified
harness/API time, not silently counted as measured model processing.

## VMM-owned tracing backend

Set `backend: "vmm_owned"` and `companion_path: "scripts/tsb-companion"` in the optional access-dirty
configuration to use the Firecracker tracer APIs. The suite freezes every
companion source/layout hash and checks those hashes before admission. The old
external observer remains available for historical replay and comparison.

This backend discovers only the VMM's PID/start identity and API socket. It does
not initialize strace, an external page-idle observer, host pagemap, or
/proc/VMM/mem. The VMM supplies authoritative RAM regions, resident PFNs with
flags/counts, RSS/PSS, separate accessed/unknown/idle and KVM/VMM dirty records,
and all bounded guest metadata bytes. Census and epoch exports are immutable
JSONL-gzip datasets; tool readings additionally retain compact region bitmaps
with hashes and relocatable sibling paths.

The host checks the selected VMM PID/start identity before each metric API call
and again after its response, and rechecks state-query identity before cleanup
release. Bounded state/error responses cannot become unbounded memory reads. A
PID reuse or VMM replacement invalidates the sample instead of joining evidence
across runtimes. VM generation and record bindings remain independently checked.

P2 starts with an explicit bootstrap epoch before the first model request.
Bootstrap search and metadata reads remain in the retained read mask and are
excluded from useful model/tool work. A rotation validates bootstrap before the
initial paused LLM interval. Tool completion pauses and rotates to the next gap;
the external decoder child begins discovery asynchronously. The next tool
admission waits for its strict seal, rotates to validate every scanned/read GPA
against the entire gap's dirty candidates, then resumes. Dirty overlap preserves
an invalid attribution receipt. Known source epoch, record, VM generation,
region and kernel-layout identity bind each live footprint to its capture.

The parent controls rotation and Resume. The child controls only bounded
metadata discovery and publication. Publication may continue after Resume, using
sealed bytes. The parent acknowledges records only after durable exports,
hashes and final consistency evidence. Structured nonmutating backpressure
permits one retry after pending finalized publication records are exported and
acknowledged; ambiguous failures invalidate observation.

Cancellation uses a separate POST /session/abort barrier after confirmed native
harness removal, with a 30-second host cleanup budget independent of the caller
deadline and container-removal budget. It interrupts normal bounded API/export
and mask-materialization work, then kills and reaps the isolated reader before
explicit capture discard and cleanup Resume. Duplicate abort requests wait for
the same serialized cleanup; they do not interrupt that cleanup or recollect an
epoch. The ACK separates released from measurement_valid, so a censored trace
can be physically safe for independent verification. Each host cleanup attempt
retains its own timing/ACK receipt.

`SIGTERM`/`SIGINT` during VMM-owned observation interrupt live API/reader work
and use the same bounded abort/release path before observer close. The open
phase remains censored, and cleanup failure remains an error. Service shutdown
does not manufacture a final epoch collection or a complete terminal seal.

The cancellation path skips final epoch collection/export and records the open
phase/group as censored; expected/missing native results are preserved. No
synthetic result or complete terminal seal is created. The VMM has no coordinator
abort operation: the abandoned epoch remains armed until VM cleanup, with no
extra reset. Previously complete immutable evidence remains retained. A prior
successful terminal seal is preserved by the host's idempotent release check. Failure to reap/discard keeps the VM held; it never
permits another valid tool window. Already sealed publication failures retain
server records and do not implicitly discard valid capture evidence. Observer
close reaps its publication children even if terminal release fails.

Boundary journals separately time metadata barriers, immutable record export,
publication waits, rotations, Pause/Resume and API-receipt persistence. The
[overhead report](memory-footprint-tracing.md#overhead-reports) retains those
parent/component envelopes, native bit-operation work, child phases and semantic
decode stages separately. It reads VMM metadata directly instead of inventing
missing legacy scan timings. Repeated/shared receipts are deduplicated; interval
unions prevent nested waits from being added twice.
Headline costs include only complete, nonfailed, noncensored records. Failed,
censored and incomplete attempt costs remain in separate completion-state
statistics, and a record completion state does not certify full trace coverage.

Useful held-LLM time is not automatically observer overhead. A decoder can run
during that interval, while only its remaining seal wait blocks the next tool
admission. Child publication can continue from sealed bytes. Host boundary
timings include API-ledger persistence, but their own journal write and helper/
network delay remain outside that envelope. The report does not subtract these
spans from tool-window timing or reconstruct uninstrumented latency.

Decoder stage/provider/closure metrics distinguish coarse categories from
process and file-backend traversal costs. Compare identical selections, sample
designs, layouts and source epochs, and retain partial/unresolved coverage.
Local in-memory comparisons isolate Python fixture costs; they do not establish
low native tracing overhead for a large workload. The full read mask qualifies
observer contamination of gap access. No complete host-idle read coverage is
inferred from exact replay or from the optimization overlay.

Suite postprocessing validates VMM-owned memory sizes and generation against the
retained initial-arm authoritative regions, without requiring legacy KVM slot
interception. Progress capture uses that same region/generation evidence and
validates the live PID/start and VM identity; VMM-owned runs do not require the
legacy `selected-slots.json` or a claimed `strace` detach. The comparison adapter retains LLM-batch identity and source
epoch/record metadata for compact VMM readings. Missing old per-scan timings
remain unavailable rather than zero; VMM boundary journals and immutable census
exports carry the new timing and backing evidence.
