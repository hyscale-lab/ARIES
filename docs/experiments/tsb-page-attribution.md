# Paused host-side page attribution

> This page describes the earlier external host-memory decoder in
> `scripts/tsb-attribution/`. Its direct `/proc/VMM_PID/mem` reads and boundary
> protocol are historical. The current VMM-owned backend uses
> `scripts/tsb-companion/` and the [access/dirty epoch protocol](tsb-access-dirty.md).
> See the [tracer source guide](memory-footprint-tracing.md) for preserved source
> and dependency setup. Private `runs/` paths below are historical examples.

This optional experimental extension to the [LLM-step footprint study](tsb-tool-pages-suite.md)
characterizes guest RAM and opportunities to reduce **host resident memory × time
between LLM tool steps**. It uses one persistent fc-ctrd VM without creating or
restoring VM snapshots. It is a characterization instrument, not an implemented
memory reclamation, compressed-memory, or prefetch policy.

The enhanced five-workload protocol first requires native decoder/reference
validation, bounded capture overhead, exact offline replay, and a successful
enhanced TB2 fix-git run. Only then does it admit the other four tasks:
prove-plus-comm, overfull-hbox, rstan-to-pystan, and
schemelike-metacircular-eval. The plan uses one independent worker per task,
native task resources/timeouts, DeepSeek Flash/high, and no automatic paid retry.
The run protocol and actual outcomes are retained under
runs/tsb-page-attribution-five. This document does not assert that those paid runs
have completed.

## Kernel metadata and bootstrap

The worker's vmlinux-5.10.239 is an unstripped ELF with symbols and embedded
IKCONFIG, but without DWARF/BTF. The configuration is extracted from its
IKCFG_ST/IKCFG_ED compressed payload. Layout constants are compiled from
Amazon Linux source tag microvm-kernel-5.10.239-261.959.amzn2, commit
1696dae7327b645d2b4b860e919e431dcda0d11c, using the embedded configuration.
Only objects containing sizeof/offsetof and constants are built; the guest
kernel is not rebuilt or modified. The available GCC 11.5 differs from the
original GCC 11.4, and olddefconfig changes only toolchain-version fields.
This is not a byte-for-byte kernel reproduction.

The base layout generators under scripts/tsb-attribution retain source archive
hashes, configuration, generated constants, ELF symbols, and runtime VMCOREINFO
comparisons. Extra owner and filename layouts cover task/thread groups, special
kernel regions, inode aliases, dentries, mount namespaces and internal filesystem
backing identities. The overlapping sizes, offsets and flag values must match;
fields absent from VMCOREINFO need independent native checks.

Bootstrap runs from the host while the VM is paused. It maps GPA to VMM virtual
address using captured KVM slots, reads /proc/VMM_PID/mem, establishes kernel
relocation/page-table translation, and checks the kernel banner and VMCOREINFO
against the supplied ELF. Layout kernel hashes must match. Bootstrap validates
process/VMA traversal before accepting the decoder. It neither invokes guest
/proc handlers nor runs a guest bootstrap collector. A matching release filename
alone is insufficient.

No guest-kernel change is required for this pinned characterization path. That
does not establish compatibility of an eventual host reclamation/compression
implementation with arbitrary Linux guests, device DMA, or other kernel builds.

## Boundary protocol

The original complete LLM tool-batch registration/result protocol remains the
measurement unit, including concurrent or harness-expanded calls. With
--attribution-config FILE:

1. At batch registration, pause the selected VM, bootstrap once if needed,
   arm the idle tracker, then resume before acknowledging registration.
2. At the final expected result, pause, finalize and retain the access bitmap,
   collect the resident backing census/sample, capture and decode metadata,
   save evidence, then resume before acknowledging the result.
3. The VM continues running during the non-tool interval. The next registration
   arms the next access window. Metadata reads occur after the preceding bitmap
   is finalized and before the next bitmap is armed.

Capture, decoding, metadata compression and artifact writes are currently
synchronous inside the pause. Retained boundary timestamps and
capture_and_decode_ns measure collection overhead; they are not tool execution
latency. The harness observation timeout bounds capture duration.

Pause stops guest CPUs, but this instrument does not prove all in-flight device
writes drained. Host reads can perturb host residency and caches. Pausing
improves consistency without removing the idle tracker's access limitations.

## File names and identities

For every decoded file-cache inode, the filename helper follows
page.mapping → address_space.host → inode aliases → dentries and parent names.
It validates inode/dentry backlinks, records filesystem/device/inode generation,
and traverses the mount namespaces of observed tasks. Available namespace paths
carry namespace inode numbers and mount IDs. Bind mounts and hard links can
produce multiple names; names are not forced into one canonical pathname.

Paths are relative to the recorded mount-namespace root, not necessarily a
process chroot. Mount-tree reconstruction does not assert visibility through
overmounts or invent overlay logical paths. Deleted, unhashed and disconnected
aliases remain labeled. A filesystem-relative path is distinguished from a
namespace-qualified path, and partial suffixes are not presented as full paths.
Empty alias lists, missing metadata and traversal limits retain explicit reasons.

Some page-cache backing has no ordinary file path. Internal bdev cache inodes
carry the actual block-device identity from inode.i_rdev, separately from the
pseudo-filesystem's device number. An ext4 allocator buddy-cache inode is
identified by a matching superblock-private s_buddy_cache pointer, not merely
by its inode number. These use internal_backing with a concrete backing_kind
and reason. Ordinary files without cached aliases remain unresolved; they are
not declared intrinsically pathless.

Per-page file_ref values address the inode-keyed files.json map within the same
capture. file-namespaces.json retains namespace/mount evidence and errors.
Every traversed metadata page is retained for offline replay.

## Categories, owners and uncertainty

The touched set remains observed accessed GPAs union unknown-access GPAs under
the requested counting convention. Rows retain observed/assumed status and the
access-unknown reason. Attribution uncertainty is a separate dimension.

The decoder walks tasks, VMAs, present user page tables, guest page metadata and
buddy free lists. It distinguishes anonymous pages, file cache, shmem, slab,
page tables, kernel stacks, reserved regions, buddy-free pages and unresolved
cases. Compound tails are classified through the validated head when possible.
Private file mappings can contain anonymous COW pages; pathname alone does not
determine category.

Ownership is a boundary association, not proof of which thread accessed or
allocated a page. Thread/address-space membership is stored in compact rosters;
per-page and VMA owner_ref/owner_refs link into the same capture's owners.json.
Process names and leader identities are retained without repeating every thread
on every page. User page-table and kernel-stack associations can identify
processes even where the page itself is not user-mapped. Shared pages can have
multiple associated processes.

Reserved pages without user mappings remain kernel-global or explicitly
unresolved, with special-region labels where available. No user process name is
fabricated. A page touched earlier can be free or reused at the boundary.
PROT_NONE/nonpresent mappings and per-CPU free lists remain outside the present
traversal. Missing traversal evidence never becomes proof of no owner or no free
pages. Dirty flags, writeback flags and refcounts alone do not establish clean,
reconstructable or safely discardable contents.

Guest semantic categories do not by themselves rule out host footprint
optimization: preserved-content eviction could in principle cover anonymous,
kernel or reserved backing as well. Hot state, sharing, pinning and restoration
cost may defeat that opportunity. This study measures relevant composition,
reuse and gap length; it does not demonstrate an operational policy.

## Resident population and memory × time

Touched-set attribution is not the whole guest-RAM population. A separate
pre-attribution census counts resident guest mappings and distinct nonzero host
PFNs in the captured KVM slots. The latter avoids duplicate aliases within the
VM but can still include backing shared with other host users. Masked/zero PFNs
and VMM RSS are reported separately; VMM RSS includes non-guest-RAM mappings.

The active workload configuration selects up to 1,024 distinct resident host PFNs
uniformly without replacement, using one representative GPA per PFN. The seed,
population, selection, host map counts and flags are retained. Sampled pages are
classified with the same metadata decoder independently of touched-set
membership. resident-sample.jsonl.gz records the category/owner/name evidence;
resident-census.json records the population and sampling design. Sparse sample
categories and uncertainty must remain explicit. A zero sample count is not
proof that a category is absent.

Compression and sampled page-payload reads are **deferred by the user**.
Workload runs set enable_resident_sampling=true and enable_compression=false.
The compression.py module contains optional historical codec machinery, but
actual workload runs do not collect compression.json or compressed payload
samples. Gzip of metadata artifacts is storage formatting, not a measured guest
memory-compression experiment. Earlier diagnostic codec data must not be mixed
into active workload claims.

For adjacent steps, the non-tool interval is next arm.begin_ns minus current
collect.resume_ack_ns. It excludes collection and next-arm pauses and includes
model/network/harness work plus possible guest background activity. It is not
pure reasoning time or proof that the guest was idle. The final step has no
invented successor interval.

Resident bytes at the left boundary × this interval is a static-footprint
byte-second model, not a continuously measured resident-memory integral.
Category estimates expand the uniform sample using its documented design.
Touched-set previous-only/new/reused pages provide a separate temporal view;
previous-only bytes × gap is a hindsight nonreuse scenario, not safe discard
or actual transfer traffic. GPA equality does not prove stable content or
allocation identity. Pause overhead, sampling uncertainty, endpoint drift and
background activity remain distinct limitations. Compression savings and codec
cost are not estimated for the deferred workload payload experiment.

## Configuration, evidence and validation

See the [adapter README](../../scripts/tsb-attribution/README.md) for invocation
and fields. Suite preparation freezes config, base/name/owner layouts and kernel
hashes; launch verifies them and records attribution source hashes. Extra layouts
are embedded in bootstrap evidence so replay does not depend on worker paths.
The observer's default behavior is unchanged without attribution config.

Per-step evidence includes reading.json, capture.json, summary.json,
pages.jsonl.gz, processes.json, vmas.json, owners.json, files.json,
file-namespaces.json, resident-census.json and resident-sample.jsonl.gz.
metadata-pages.bin.gz plus metadata-index.json retain the decoder-read 4 KiB
pages for replay. These copies are not a restorable VM or a dump of every
touched page payload. Preserve them with matching code/layout provenance.

Before paid workload interpretation, require boundary/failure tests, native
ground-truth checks for anonymous/shared-file/COW mappings and paths, owner
associations and special pages, scalable capture validation, and exact replay.
Known fixture markers are diagnostic payload reads and are not workload payload
sampling. Validate attribution correctness separately from idle-tracker coverage:
a missed known access remains a measurement failure, even if a separate reference
capture correctly classifies the page. Preserve failed attempts and their scope.

All execution, builds, tests and analysis run on workers. Copy code recovery
artifacts, metadata, raw logs and reports to the control node, excluding
credentials. Record harness/evaluator outcomes, boundary and attribution
coverage, runtime absence and cleanup before claiming a task succeeded.
A fixture pass is not evidence of paid workload completion or generality.

Source references:
- [Firecracker kernel policy](https://github.com/firecracker-microvm/firecracker/blob/main/docs/kernel-policy.md)
- [Matching Amazon Linux source](https://github.com/amazonlinux/linux/tree/1696dae7327b645d2b4b860e919e431dcda0d11c)
- [Linux VMCOREINFO](https://docs.kernel.org/admin-guide/kdump/vmcoreinfo.html)
