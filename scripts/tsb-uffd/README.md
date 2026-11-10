# Per-restore page faults for tool sandbox experiments

This experimental Python handler uses the existing Firecracker 1.13.1 UFFD
backend. It serves exactly one 4096-byte page per missing-page fault, records
its stable snapshot offset, and records caller-controlled phase/tool labels.
No page contents are written to the event log. The backing snapshot itself is
private guest memory and must remain private.

Run on the prepared worker as an account able to read private snapshots and
receive the Firecracker UFFD descriptor:

```sh
python3 scripts/tsb-uffd/handler.py --socket /private/uffd.sock \
  --control /private/control.sock --memory /private/memory \
  --log /private/events.jsonl
```

Wait for both sockets and a successful control `status` before submitting the
native CreateVM request with `MemBackend{BackendType:"Uffd",BackendPath:...}`.
The socket path must be reachable from Firecracker's mount namespace. The
pinned containerd stack already forwards this backend; the existing ARIES
checkpoint restore otherwise hardcodes `File`.

```sh
python3 scripts/tsb-uffd/control.py --socket /private/control.sock
python3 scripts/tsb-uffd/control.py --socket /private/control.sock \
  --phase tool --tool-id call-1
# Execute the exact tool operation, then delimit its end before snapshotting.
python3 scripts/tsb-uffd/control.py --socket /private/control.sock --phase snapshot
```

The handler initially uses `restore`. The native shim restores with
`ResumeVM=true`, and guest/VMM activity can fault pages before tool dispatch.
A phase acknowledgement is a host handler-clock boundary, not an atomic guest
CPU barrier. A queued fault can straddle the boundary. Serialize experimental
tool calls and retain timing records rather than implying causal attribution
for overlapping calls.

Each event records a region index, host page address, snapshot byte offset,
page index, triggering host fault flags and copy outcome. Host virtual
addresses change across restores. Compare snapshot offsets for the same VM
memory layout. The native handshake does not contain guest physical addresses;
optional `--guest-bases '[...]' --guest-bases-source '...'` adds GPAs only when
the caller supplies independently verified native region bases in mapping
order. The x86 memory hole means snapshot offsets must not be relabelled GPAs.

This is a first-touch trace after restoring cold guest RAM. Once populated, a
page does not fault on subsequent accesses. Thus `tool` events alone omit
pages already populated during restore, even if the tool accesses them.
`restore + tool` describes the restore-to-tool-completion envelope, including
VM kernel/device/background activity. Neither is automatically the exact set
of pages caused exclusively by the tool. Preserve both sets when comparing
turns. A read then write to one page yields only its initial fault; the write
flag is not a complete write set, and it is not yet validated as guest
instruction access type under KVM. Idle tracking likewise observes a combined
read-or-write access set, not separate read and write sets.

The handler must remain alive through subsequent snapshot creation: a full
snapshot reads previously untouched pages and can generate many additional
faults. Keep those in the `snapshot` phase and exclude them from tool sets.
Exit is automatic after the exact Firecracker peer PID/start identity dies;
`--stop` control is rejected while that peer lives. A handler error fails the
experiment and may leave Firecracker blocked, so the owner must inspect the
handler process and tear down the owned VM on failure. Snapshot files must
remain available until the handler ends. No speculative prefetch is performed.
Balloon REMOVE events and non-4096-byte mappings fail explicitly; do not enable
balloon or host huge pages for this experiment.

Validation commands (worker only):

```sh
python3 -m unittest discover -s scripts/tsb-uffd -p test_handler.py -v
sudo python3 scripts/tsb-uffd/test_native.py --output /absolute/new/output-dir
```

The native test compiles a small C fixture, passes a real UFFD over SCM_RIGHTS,
faults read/write pages, checks exact restored bytes, exercises two tool phases,
and proves repeated reads do not create extra faults. This validates the host
handler protocol, not Firecracker or complete tool attribution. Actual VM and
LLM runs require their own evidence.

Inspected native sources:

- Worker `.cache/worker-stack/firecracker-containerd/runtime/service.go:593`:
  forwards memory backend and resumes restored VM.
- Worker `pkg/deployment/vhive/checkpoint.go:370`: existing File backend.
- [Firecracker 1.13.1 UFFD persistence](https://github.com/firecracker-microvm/firecracker/blob/v1.13.1/src/vmm/src/persist.rs):
  native mapping schema, missing-page registration and FD handshake.
- [Firecracker 1.13.1 handler example](https://github.com/firecracker-microvm/firecracker/blob/v1.13.1/src/firecracker/examples/uffd/on_demand_handler.rs)
  and [utilities](https://github.com/firecracker-microvm/firecracker/blob/v1.13.1/src/firecracker/examples/uffd/uffd_utils.rs).
- [Snapshot UFFD documentation](https://github.com/firecracker-microvm/firecracker/blob/v1.13.1/docs/snapshotting/handling-page-faults-on-snapshot-resume.md).

## Worker compatibility discovered by the VM pilot

The first actual restore failed before the UFFD handshake: the pinned vHive SDK
serializes a fork-specific `container_snapshot_path` field that stock
Firecracker 1.13.1 rejects. The generated SDK field is required and lacks
`omitempty`, so merely passing an empty path cannot fix it.

For this experiment, `stock-firecracker-sdk.patch` changes that field's JSON tag
to `-` in a private SDK copy under `.cache/worker-stack/`; the native shim is
rebuilt against that copy and installed. The original shim is preserved there.
The main ARIES `go.mod` dependency resolution and daemon binary are unchanged. This is an experimental
stack patch, not evidence that the unmodified pinned stack supports this path.
`runs/tsb-tool-pages/uffd-compat` retains the source patch, module replacement,
build output, original/installed binary hashes, and provenance. The provider
records the container disk source when suspending and checks exact equality
before UFFD resume: the snapshot retains original absolute device paths and
this patch does not remap disks. A changed disk path fails rather than restoring
against another device.

The coordinator accepts either argv arrays or step objects containing an
`operations` array. It snapshots once after all operations in a step, restores
before the next, and records comparisons in `comparison.json`:

```sh
sudo python3 scripts/tsb-uffd/run.py \
  --commands runs/tsb-tool-pages/pilot-commands.json \
  --output runs/tsb-tool-pages/uffd-pilot-NEW
```

Each restored VM checks the same boot ID, persistent disk marker and live
background PID in a separate `restore_validation` phase. That phase is included
in the explicitly named restore-to-tool-completion envelope, and is excluded
from tool-only first-fault sets. It is diagnostic activity and is not attributed
to the tool itself.

After a run ends, `python3 scripts/tsb-uffd/compact.py OUTPUT` losslessly
compresses terminal fault logs. It verifies the decompressed SHA-256 before
removing the uncompressed duplicate and records sizes/checksums in
`compression.json`. The coordinator summarizer accepts either JSONL or gzip.

The retained SDK compatibility patch uses zero context to avoid whitespace-only
patch lines. Apply to the exact pinned SDK with
`git apply --unidiff-zero stock-firecracker-sdk.patch`; retain the model file
hashes and use only with an unchanged retained disk source.
