# Bounded access-observer calibration

These utilities establish feasibility; they do not admit the benchmark pipeline
or turn resident/dirty counters into access sets. `quality-gate.json` was declared
before the first calibration. The executable target gate additionally requires
zero unknown target pages; this stricter identity requirement existed before the
first target measurements (003) and is not relaxed after observing non-LRU pages.
A missing or failed required gate blocks whole-VM
H1/H2 admission. Raw metadata belongs under ignored `runs/tsb-memory-footprint/`.

Compile the fixture on the worker:

```sh
cc -static -O2 -Wall -Wextra -Werror -pthread scripts/tsb-access/guest_fixture.c \
  -o runs/tsb-memory-footprint/access-calibration/guest-fixture
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s scripts/tsb-access -p test_idle.py -v
```

The fixture needs guest `CAP_SYS_ADMIN` to expose PFNs from pagemap. That is a
calibration-container capability only, not a benchmark-provider policy change.
It prefaults a 4 MiB range and reports guest physical identities, never payload
bytes. Its persistent stdin/stdout protocol supports no-touch, read, write,
free/reallocate, concurrent/background readers, and file-cache reads. The file
exercise uses `pread` against a prefaulted mapped file; it exercises the guest
kernel path but does not isolate all guest-kernel pages as known targets.

Run one fresh VM with the native calibration driver, supplying its current
containerd parent PID and a fresh artifact path:

```sh
sudo env PYTHONDONTWRITEBYTECODE=1 python3 scripts/tsb-access/calibrate.py \
  --extended --trace-parent CONTAINERD_PID --vm-id calibration-RUN \
  --vm-info runs/tsb-memory-footprint/access-calibration/vm-RUN/vm-info.json \
  --output runs/tsb-memory-footprint/access-calibration/primary-RUN.jsonl \
  --guest-command bin/tsb-vm \
  -fixture runs/tsb-memory-footprint/access-calibration/guest-fixture \
  -output runs/tsb-memory-footprint/access-calibration/vm-RUN
```

The tracer temporarily attaches before provisioning and follows native children.
It retains only successful `KVM_SET_USER_MEMORY_REGION` fields and detaches
before measuring targets. Other ioctl text is discarded in memory. A failed,
missing, ambiguous, or interleaved/truncated registration is not permission to
infer mappings from VMA size. API machine configuration must independently show
2 vCPUs and 4096 MiB. The VM start identity and KVM GPA-to-HVA slots identify
pages; host PFNs are transient lookup keys only. Fixture identity changes on
free/reallocate are retained separately from slot membership.

The primary observer reads host pagemap, kpageflags, kpagecount and page-idle.
It only reports stable, exclusive, resident LRU base pages; compound pages are
explicit unknowns. It never expands a huge-page hit into invented 4 KiB hits.
This implementation does not yet aggregate compound folios and therefore cannot
claim full coverage when they occur. Host/VMM activity can also clear idle.
Endpoint checks cannot exclude transient isolated/locked pages or mapping A-B-A
changes, nor accesses to pages resident only between checks. Whole-resident
samples retain endpoint coverage and observed identities, not proof of complete
interval coverage. Saved `maps`/`smaps` are metadata, not memory contents.

The initial 36 tests cover six schedules at two waiting intervals and three
repetitions. The extended run adds reuse, concurrent and background activity,
file-cache reads, and two full-resident samples. Each sample records actual arm
and scan time; the configured wait is not an achieved sampling period. On/off
fixture durations are paired diagnostics, not population latency estimates.
The full observer CPU measurements include metadata enumeration and access-bit
operations. Target-only cost cannot certify full-observer overhead.

One fallback is inspected: guest page-idle. An unavailable interface blocks that
fallback. Guest `smaps` referenced totals, soft-dirty bits, faults and RSS cannot
supply the required read/write page sets and are not substituted. No kernel or
hypervisor redesign is included.

## Source audit

Linux 6.8 [page_idle.c](https://github.com/torvalds/linux/blob/v6.8/mm/page_idle.c)
calls `ptep_clear_young_notify` / `pmdp_clear_young_notify`; only LRU folio heads
are eligible. The [idle tracking documentation](https://github.com/torvalds/linux/blob/v6.8/Documentation/admin-guide/mm/idle_page_tracking.rst)
explains silently skipped page classes and huge-page head granularity.
[KVM's clear_young notifier](https://github.com/torvalds/linux/blob/v6.8/virt/kvm/kvm_main.c)
explicitly omits secondary TLB flushes, so recurrence recall must be tested.
The worker's installed 6.8.0-147 headers confirm the notify calls; upstream source
alone is not proof that all Ubuntu backport implementation details match.
[Firecracker 1.13.1 x86 memory layout](https://github.com/firecracker-microvm/firecracker/blob/v1.13.1/src/vmm/src/arch/x86_64/mod.rs)
contains the MMIO hole, but actual KVM registrations remain the mapping evidence.
