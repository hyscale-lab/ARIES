# Historical fix-git page-set pilot

> This page records the earlier single-call, single-worker fix-git pilot and
> separate UFFD experiments. Its step definition, overlap rejection, resource
> overrides and admission scope are historical. For the current six-worker
> LLM-response batch protocol and native task resources/budgets, see the
> [Terminal-Bench LLM-step suite](tsb-tool-pages-suite.md). The current VMM-owned
> access/dirty implementation and reproducible source setup are described in the
> [tracer source guide](memory-footprint-tracing.md).

The pilot's measurement unit was a **step**: one LLM-issued tool call, potentially
expanded by its harness into several consecutive sandbox operations. A session
was the end-to-end task; a turn was one user–harness interaction. Each pilot
occurrence admitted one task instruction and recorded one turn per session.
The earlier high-frequency memory-study admission gate is not this experiment's
objective. This study logs and compares observed page sets at step boundaries;
it does not claim a complete physical working set or benchmark H1/H2 result.

## Historical protocol

The native OpenClaw `before_tool_call` hook awaits a host observer acknowledgement
before executing the tool. The nonsynthetic `tool_result_persist` hook is a
synchronous completion barrier before the tool result becomes available to the
next model request. Native `after_tool_call` delivery is detached, so it cannot
provide that barrier. Session, turn, native run and LLM tool-call IDs are recorded;
all sandbox operations within the tool execution share a single observation
window. Native SSH cwd preparation is deferred until execution by its backend.
Live bridge evidence must corroborate these boundaries.

The first implementation finalizes observations synchronously. After a page set
is frozen, persistence and preparation can overlap LLM thinking, provided the
next step cannot begin before tracking is ready. There is no claim that this
optimization is implemented. No command body, native tool result, verifier,
retry policy or process-lifetime policy is rewritten. Parallel/overlapping steps
are reported as unmeasurable on one shared VM, not silently serialized.

## Idle-page path

`scripts/tsb-pages/serve.py` captures successful KVM RAM-slot mappings during
native VM creation, detaches strace before the first observed step, and arms the
host page-idle bitmap. It batches sparse contiguous kernel metadata reads,
inspired by Brendan Gregg's `wss-v2`. Collection saves exact observed GPA lists,
unknowns and CPU/wall timestamps. Re-arming before the next step clears tracking
history without discarding guest memory or disk state. LLM thinking time lies
outside the armed execution window.

These are combined read-or-write observations, not separate read and write sets.
Pages must remain resident, exclusive, LRU base pages with stable host PFNs.
Newly resident, vanished, shared, non-LRU and changed mappings are explicit
unknowns. Guest/VMM/background activity can contribute accesses. KVM TLB effects
can cause missed accesses. Collection is a scan interval, not an instantaneous
CPU-wide bitmap snapshot; its beginning/end are recorded. GPA equality across
steps identifies physical guest locations, not necessarily the same allocation.
The comparison does not equate page numbers from different VMs.

## Kernel bitmap I/O limit and correction

Linux 6.8 kernfs limits binary sysfs reads and writes to one host page per
system call. On these workers that is 4096 bytes, or 512 64-bit idle-bitmap
words. The earlier shared 8192-word batching could request 65536 bytes and
treated kernel clipping as a short-I/O failure. The corrected collector limits
only idle-bitmap reads and writes to 512-word runs; `/proc` pagemap, kpageflags
and kpagecount keep their larger batches. Exact-length checks remain.
See the [Linux 6.8 kernfs implementation](https://raw.githubusercontent.com/torvalds/linux/v6.8/fs/kernfs/file.c).

Safe worker validation reproduced 8192-byte requests returning 4096 bytes and
transferred 2049 words with five bounded reads and writes. It used only zero
write masks and an `/proc/iomem`-verified non-System-RAM hole. Zero mask bits
skip folio/PTE updates in the
[page-idle implementation](https://raw.githubusercontent.com/torvalds/linux/v6.8/mm/page_idle.c),
so active guest PFNs were unaffected. The private evidence is
`runs/tsb-tool-pages-suite-003/validation/idle-bitmap-fix/safe-kernel-integration.json`.

Earlier completed readings remain their original partial VM-page evidence:
the old collector emitted them only after full-write/full-read checks. A failed
arm produced no registered batch reading. Missing later windows and derived
unregistered-call errors remain failures, never zero footprints. Original
artifacts and failed attempts are retained.

## Snapshot/UFFD path

`scripts/tsb-uffd/run.py` restores the same logical VM and retained writable disk
once per step. A handler supplies exactly one 4 KiB snapshot page per missing
fault. All constituent operations run before the next snapshot. Restore, identity
validation, tool execution and snapshot creation have separate phase labels.
Snapshot offsets are stable within the retained VM memory layout; they are not
renamed GPAs across the x86 memory hole.

A UFFD first-fault set is not automatically the exact tool access set: pages
populated during restore do not fault again when a tool reuses them. Both the
tool-only first-fault set and the restore-to-tool-completion envelope are retained.
The latter includes restore activity. Initial fault flags are not a complete
write set. Full snapshot creation itself touches otherwise untouched pages;
those faults are excluded from tool comparisons. The handler must keep serving
until VMM teardown, and checkpoint files must not overwrite live backing files.

The stock Firecracker 1.13.1 API rejects the pinned SDK's fork-only
`container_snapshot_path` JSON field. The worker pilot uses an explicitly
recorded one-line SDK compatibility patch in its private shim build to omit that
field. ARIES's research-only `RestoreMemorySocket` option checks the retained
disk source against the checkpoint before restoring; disk-path relocation is not
supported by this experiment. The main Go module dependency pin is unchanged.

## Historical scope and evidence

Use the existing worker only; copy all logged data and final reports back to the
control node. Credentials and live snapshot backing files are not log artifacts.
All source revisions, native stack patches, failures, raw page sets, model usage,
cleanup and validation logs must be retained. Pilot resource shape is fixed at
2 vCPUs and 4096 MiB with native API verification. Deterministic mechanism pilots
precede the native OpenClaw smoke and a single bounded paid fix-git session using
the previously authorized `deepseek-flash`, high thinking effort. Paid session
wall deadline is the profile's 900 seconds, with no automatic paid retries.
No broader task sweep is admitted by a successful pilot.

Profiles are under `profiles/experiments/tsb-tool-pages`. Worker-specific
`harness.step_observation.plugin_dir` and observer URL must identify the installed
plugin and dedicated observer. The plugin is not shipped to the tool sandbox.
Reports distinguish deterministic mechanism commands, mock-model native harness
steps, and genuine paid-model steps. Passing transport and fixture checks does
not certify exact page-access coverage.

## Executed result

One paid `fix-git` session completed with `deepseek-flash` and high thinking:
11 LLM tool-call steps, 41 native bridge operations, and 12 model requests.
The original verifier returned reward 1; harness stop, bridge revocation,
isolation and task/shared cleanup succeeded. Native transcript IDs, hook
acknowledgements, operation intervals, result persistence and subsequent model
requests reconcile in `native-paid-001/boundary-audit.json`. No paid retry ran.

Ten steps executed sandbox operations. Their observed read-or-write sets contain
7,594–17,541 guest pages (4 KiB each). The remaining failed native tool call made
no sandbox operation; its 559 observed pages reflect VM activity during that
interval, not proven command accesses. It is retained, not dropped. The union
across all eleven observed sets is 21,417 pages. Raw unknown lists and pairwise
intersection/new/lost/Jaccard values are retained alongside each set. Adjacent
pairs where both steps executed sandbox operations had Jaccard values
approximately 0.40–0.83; this is observed
GPA membership overlap, not certified allocation identity or complete access
coverage.

These particular step intervals were short: 0.116–1.253 seconds. Median arm and
collection times were 0.980 and 2.005 seconds, with 2.992 CPU-seconds of observer
work per step. The per-step protocol works, but this Python observer remains
intrusive for this workload. An LLM-issued tool call is not inherently long.
Collection still has a finite scan window, and unknown/newly resident pages
prevent treating these counts as exact complete footprints.

The native mock validation retained two steps expanding to five and four bridge
operations. All nine were enclosed by their corresponding step windows. The
first native mock failed observation because the native plugin loader activated
a second registration whose hook closures did not share the service state. The
corrected plugin uses the native process's shared service state; live metadata
and the regression fixture verify this. Both attempts remain logged.

Four deterministic UFFD steps completed in 438.4 seconds. Boot identity, a
background PID and a disk marker survived all restores. Tool-only first-fault
sets contained 8,747 / 4,706 / 671 / 8,911 pages; restore-through-tool envelopes
contained 14,831 / 13,082 / 15,774 / 15,147. Identical read steps 2 and 3 had
Jaccard 0.013 for tool-only faults but 0.611 for the envelopes because restore
validation had already faulted pages. Full snapshot creation faulted more than
one million additional pages each cycle; those events are excluded from tool
sets. All raw fault logs are losslessly compressed and verified by decompressed
SHA-256. This pilot does not integrate UFFD into the paid native harness run.

The final overview is `runs/tsb-tool-pages/report.html`; comparison and UFFD
reports link the raw data and audit records. All artifacts and source recovery
files are copied to the control node, with a SHA-256 transfer manifest. The
credential is excluded. Artifact byte counts and release-check results belong
in the run status rather than this methods document.

## Unknown-inclusive report revision

At the user’s request, primary report counts and Jaccard comparisons now use
`observed touched GPAs ∪ all recorded unknown GPAs` for each step. This is an
explicit counting assumption, not proof that every unknown page was accessed.
Raw classifications are unchanged. JSON retains `observed_only_pairs`, and HTML
shows the original matrix beneath the unknown-inclusive matrix. Earlier executed
result statistics above describe observed-only sets; revised statistics are in
`study-status.json` under `unknown_inclusive_analysis`. No workload rerun is needed.

## Memory page footprint collection methodology

The reports embed the shared [PFN methodology](../../scripts/tsb-pages/methodology.html),
explaining arm → full LLM tool-call execution → collect → re-arm, unique-page
footprint size and Jaccard comparison. Expandable technical detail covers captured
KVM slots, GPA-to-HVA translation, privileged host pagemap PFN
extraction, kpageflags/kpagecount eligibility, idle bitmap addressing, native step
barriers, mapping stability checks, saved GPA identities and unknown inclusion.
Host PFNs were transient collector state, not retained full mapping tables.
The UFFD pilot uses snapshot offsets from the native fault-region handshake, not
pagemap PFNs. The methodology distinguishes this from the paid idle-tracked run.
