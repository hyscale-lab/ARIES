# Paused host-side page attribution

This optional observer uses one persistent fc-ctrd VM. It does not snapshot,
restore, or modify the guest kernel. It characterizes opportunities to reduce
host resident memory × time between LLM tool steps; no reclamation, eager-fetch,
or compressed-memory policy is implemented.

At batch registration: pause → host-only bootstrap once → arm idle tracker →
resume. At the final expected result: pause → finalize bitmap → resident backing
census/sample → metadata capture and decoding → save → resume. Native parallel
calls remain one batch. The VM runs between batches. Capture/decoding/writes are
currently synchronous inside the pause.

## Configuration and admission

Use serve.py --attribution-config FILE, or suite.py --prepare with
--attribution-config FILE and an explicit task selection. The active five-task
protocol gates the other four tasks on enhanced native validation, bounded
capture, replay, and fix-git success. It uses one worker per task and no automatic
paid retries. Experiment protocols and captured data are supplied separately.

Config is passed to GuestDecoder(pid, vm_id, slots, config):

| Field | Meaning |
|---|---|
| kernel_image | Exact cached guest ELF |
| layout_path | Base kernel types/symbols |
| names_layout_path | Additional inode/dentry/mount/filesystem offsets |
| owner_layout_path | Additional task/thread/special-region offsets |
| enable_resident_sampling | Enable distinct resident-host-PFN census/sample |
| resident_sample_pages | Sample size, 1,024 in the workload protocol |
| enable_compression | **false for active workloads; payload sampling/compression deferred** |

The constructor embeds extra layouts as names_layout_data/owner_layout_data.
Preparation freezes all input hashes and attribution Python source hashes;
launch verifies them. Decoder methods bootstrap() and capture(reading,
output_dir) return JSON evidence; close() releases handles.

The adapter resolves --api-sock from the selected VMM through /proc/PID/root or
cwd. Optional socket_path or socket_template ({vm_id}, {pid}) overrides discovery.
PID/start identity is checked. api_timeout_seconds defaults to 10. The existing
harness observation request timeout is 120 seconds; capture must fit within it.

## Naming and owners

file_names.py follows inode aliases, names, parent dentries and task mount
namespaces. It returns filesystem/device/inode identity and multiple namespace/
mount-qualified paths where present. Deleted, unhashed, disconnected, partial,
missing and limited traversals stay explicit. Overmount visibility, overlay
logical aliases and process chroots are not invented.

Internal block-device cache reports actual inode.i_rdev identity; ext4 allocator
buddy cache requires its superblock pointer match. These are internal_backing,
not missing ordinary filenames. Generic empty alias lists remain unresolved.

owners.py stores thread/address-space membership once in compact rosters.
Per-page and VMA references point into that capture's owners.json. Associations
include present user mappings, user page tables and identifiable kernel stacks;
reserved/global kernel state is labeled without fabricating a user owner.
Ownership is boundary state, not causal access or allocation attribution.

## Evidence and metric

Core files: attribution/bootstrap.json, boundaries.jsonl, and each
step-NNNNNN/{reading.json,capture.json,summary.json,pages.jsonl.gz,processes.json,
vmas.json,owners.json,files.json,file-namespaces.json}. The metadata GPA index and
gzip binary retain all decoder-read pages for replay without a live VM.

Resident census/sample files describe one uniform sample without replacement
over distinct nonzero resident host PFNs in KVM RAM slots. Selection is
independent of touched-set membership. A representative GPA supplies category/
owner/name metadata; zero/masked PFNs, sharing evidence and VMM RSS stay separate.
The active workload path does not deliberately read sampled page payloads and
does not produce compression.json or compressed-sample-data.bin. Gzip metadata
storage is not a guest-memory compression measurement.

Interstep time is next arm.begin_ns minus current collect.resume_ack_ns.
Resident bytes × that gap is an endpoint/static-footprint model, not a continuous
integral or measured reclaimed memory. The final step is censored. Keep sample
uncertainty, touched-set transitions, background activity and pause overhead
separate. See the [methodology](../../docs/experiments/tsb-page-attribution.md)
for complete limits.

Finalized access evidence survives attribution failure. Errors and resume failures
remain explicit. Even an uncertain pause timeout gets a resume attempt; close
retries a failed resume. Resume failure is fatal and requires native cleanup.
Pausing does not prove all device writes drained. Host reads can perturb residency
and caches, and idle-tracker limitations remain.

## Worker validation

Run on a worker:

    python3 -m unittest discover -s scripts/tsb-attribution -p 'test_*.py' -v
    python3 -m unittest discover -s scripts/tsb-pages -p 'test_*.py' -v

Require native ground truth and exact metadata replay before interpreting paid
workloads. The reference fixture validates mappings, COW, file names and owner
associations separately from observed-access coverage. Scalable 2-GiB capture
needs its own timing check. A native fixture is not a workload score, generic
Linux compatibility proof, or permission to omit failed experiments.

## Kernel layouts and native fixture

The committed `layouts/linux-5.10.239/` directory contains the recovered base,
file-name and owner layouts, ordered offset names, and compiler input. The
decoder checks the kernel ELF hash against the base layout before attribution.
Tests use these committed JSON files and require no captured experiment data.

To regenerate offsets from the matching prepared Amazon Linux kernel source:

```sh
python3 scripts/tsb-attribution/layout_generate.py \
  --output-dir .cache/attribution-layout \
  --kernel-source /path/to/prepared/linux-source
```

Compile the generated `kernel/layout_offsets.c` with that kernel's configuration
and extract its `attribution_layout` values as raw little-endian unsigned longs.
Decode those values and validate overlapping VMCOREINFO fields explicitly:

```sh
python3 scripts/tsb-attribution/layout_decode.py \
  --offsets /path/to/offsets.bin \
  --names .cache/attribution-layout/offset-names.json \
  --kernel-image /path/to/vmlinux-5.10.239 \
  --output .cache/attribution-layout/base-layout.json
python3 scripts/tsb-attribution/layout_validate.py \
  --layout .cache/attribution-layout/base-layout.json \
  --anchors /path/to/anchors.json \
  --output .cache/attribution-layout/vmcoreinfo-validation.json
```

Native validation runs only on a configured Firecracker worker. Compile
`fixture.c` for the guest, supply an attribution config with the matching kernel
and committed layout paths, and choose a new output directory:

```sh
python3 scripts/tsb-attribution/validate_native.py runs/attribution-native-check \
  --config /path/to/attribution-config.json \
  --fixture /path/to/compiled/fixture \
  --driver bin/tsb-pages-vm
```

The config keys `layout_path`, `names_layout_path`, and `owner_layout_path` can
point to the corresponding committed layout JSON files. Kernel images, compiled
fixtures, captured anchors and native outputs are runtime inputs and artifacts.
