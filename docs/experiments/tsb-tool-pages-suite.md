# Terminal-Bench memory footprints by LLM reasoning step

> This page preserves the earlier six-worker host-page-idle suite protocol. Its
> LLM-response batch definition remains relevant, but its collection backend and
> run-specific admission decisions are historical. Use [access/dirty epochs](tsb-access-dirty.md)
> for the VMM-owned backend and the [tracer source guide](memory-footprint-tracing.md)
> for current code paths and setup. Private run paths below are historical
> evidence locators, not inputs required to build the tracer.

## Measurement unit

The user-defined collection and optimization window is **from the LLM's tool
request until the harness returns the results to the LLM**. One window covers
the entire tool-containing assistant response, including every requested call
and every harness-expanded operation, whether sequential or concurrent.

The hierarchy is session (end-to-end task), turn (user-harness interaction),
LLM reasoning step (tool-request/result phase), then native tool calls and bridge
operations. LLM inference itself lies outside the window. A temporary absence
of active native calls does not end a window if the same LLM response still has
outstanding results. Final assistant answers with no tool requests create no
tool-execution window.

Each window has one shared page set. Individual tool identities and timings are
supporting evidence; unique per-tool causal page footprints are not claimed.

## Native boundary protocol

The synchronous OpenClaw before_message_write hook sees the complete assistant
tool request before native execution. It registers the normalized native call
IDs and names, without duplicating arguments or reasoning text. The observer
arms the page-idle bitmap once before acknowledging that registration. The hook
returns without changing the native message.

Native before_tool_call hooks record admissions without resetting the bitmap.
The synchronous tool_result_persist hook records each expected result.
Intermediate acknowledgements leave collection pending, including gaps between
sequential calls. The final expected result freezes the page set before its
acknowledgement allows the next model request. Immediate native error results
can lack a before-tool admission; they remain explicit members of the batch.
Duplicate, unexpected, synthetic or missing results cannot silently produce a
successful measurement.

The native scheduler awaits the complete tool batch and result delivery before
its next model request. Audits must corroborate the registered call list against
the persisted assistant response, associate every bridge operation with one
LLM-step window, and verify collection before the next model dispatch.
Per-call result-persistence timestamps can follow completion of all parallel
executions; they are not individual execution-completion timestamps.

A native background command can outlive the result that reports it as running.
Audit its bridge admission against the LLM window, and retain its overlaps with
later windows and intervals outside them. Command lifetime does not extend a
completed LLM step. A native deadline that interrupts final result persistence
is explicitly censored; a raw final bitmap alone does not prove delivery to the
LLM. Exclude such a window from primary complete-window metrics.
A declaration-only terminal request can also be censored when native deadline,
isolation, cleanup, matching observer registration/shutdown identities and all
preceding window checks corroborate it. It has no footprint and does not prove
native persistence or tool execution. Preserve the shutdown error. Independent
mapping/resource, comparison or protocol failures must still stop admission.

## Page collection and interpretation

The underlying mechanism remains the [idle-page pilot](tsb-tool-pages.md):
capture the VM's KVM RAM slots, detach the startup tracer, enumerate guest RAM
backing through the VMM's pagemap, and mark eligible host PFNs idle. At the end
of the LLM window, read idle bits and recheck identities. Guest memory and disk
state persist; re-arming clears tracking history, not sandbox state.

Primary counts use observed touched GPAs union all recorded unknown GPAs, as
requested. Unknown inclusion is a counting assumption, not proof of access.
Retain observed, idle, unknown and nonresident classifications. Compare guest
page addresses only within one VM. Jaccard matrices have one row per LLM
reasoning step, never duplicated rows for members sharing a footprint.

Protocol validity does not certify complete access coverage. Finite scans, KVM
translation effects, residency/PFN changes and background activity remain
limitations. Guest resident RAM is not the complete attributable host-memory
denominator. Record observer overhead, incomplete windows and verifier outcomes
separately. One session per task does not establish run-to-run reproducibility.

## Optional page attribution

The [paused host-side attribution extension](tsb-page-attribution.md) adds
boundary-state categories and process mappings to each counted GPA. It requires
matching kernel metadata and native decoder validation. Its pause/capture costs,
partial ownership coverage and decode errors are separate from access evidence.
The existing footprint-only suite does not retroactively contain page contents
or attribution metadata.

## Suite execution

Collect all 89 tasks in Terminal-Bench 2.1 at pinned revision
7131e4375048a0e408a8fb404b5f499d726b695b. Sorted task IDs are assigned round-robin
to six workers: amd205, amd235, amd234, amd225, amd241 and amd218 at
utah.cloudlab.us. Run one task at a time on each independent host.

Use native task CPU, RAM and agent/verifier timeouts. Tasks declare 1, 2 or 4
CPUs; 2, 4 or 8 GiB RAM; no GPU; and 10 GiB storage. Use OpenClaw with
deepseek-flash and high thinking. Each task gets one initial main-sweep session,
with no automatic paid retries. Any manually justified corrective attempt must
retain the previous attempt and its reason.

After an admission stop, prepare a fresh artifact root with an explicit
selection JSON containing task_ids, a reason, and references to retained prior
outcomes. The driver freezes the selection contents, source path and SHA-256 in
the manifest. It retains the full 89-task catalog but prepares and executes only
the selected IDs assigned to that worker. Previously attempted tasks are not
silently retried. Corrective attempts require a separately recorded selection.
Measurement or boundary-audit failures stop subsequent task admission after
the current native lifecycle and cleanup finish. A separately verified native
deadline censor is retained as incomplete, with complete preceding windows
reported separately; it does not by itself stop admission of unrelated tasks.

All execution, validation and analysis run on workers. Copy logs, reports and
source recovery artifacts to the control node throughout the study; exclude
credentials. Keep all 89 tasks in the coverage inventory, including preparation
failures, task failures, timeouts, censored observations and missing measurements.
A verifier failure does not itself invalidate an otherwise measured window.
Missing data must not be represented as a zero footprint.

The driver under scripts/tsb-pages records source and binary hashes, pinned task
metadata, per-task profiles, raw observations, native harness/bridge evidence,
comparison reports, boundary audits and lifecycle outcomes. Verify worker runtime
absence after each task. Live progress and a complete inventory are not proof of
completed collection.

## Validation and retained admission failures

Before the main sweep, validate both concurrent and sequential tool calls from
a single LLM response. Both must produce one window, one arm and one collection,
with no interior reset and with collection completed before the next model
request. Native mock fixtures must preserve actual tool and bridge behavior;
fixture success is not a benchmark task score.

The first suite admission used revision ad8eb6d and the pilot's per-call observer.
The first adaptive-rejection-sampler assistant response contained two exec calls.
The observer rejected overlap and invalidated observations. Further admission
stopped, the active run was canceled through native ARIES cleanup, and owned
runtime absence was confirmed. This paid failure is retained in
runs/tsb-tool-pages-suite-001 rather than counted as a successful collection.

The other five initial shards failed only during setup because helper binaries
were omitted from provisioning. They made no paid model calls. The complete
four-binary runtime was subsequently copied and actual ARIES setup passed on
each host; the initial setup logs remain retained.

A busy-period prototype passed a real concurrent native fixture, but it could
split sequential members of one LLM response. An experimental segmented
prototype also exposed additional read/re-arm gaps inside running calls.
Their source and evidence are retained as intermediate validation artifacts;
neither is the selected LLM-window definition.

## Derived comparison provenance

Pairwise comparisons may use bounded dense GPA bitmaps with a sparse fallback
and cached symmetric intersections. These are exact set-cardinality calculations
on retained observations, not changes to collection. Each comparison records its
postprocessor source hash in a separate sidecar; numerical JSON and HTML remain
compatible with the reference implementation. Record analysis-source rollouts
separately from the source pinned when an already running collector was launched.
