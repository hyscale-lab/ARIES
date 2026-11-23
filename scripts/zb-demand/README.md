# Demand faults in stock KVM

This probe targets the AMD nested-page-fault path on the study's Ubuntu 6.8
x86_64 host. It uses KVM and core MM BTF and was exercised with bpftrace
0.20.2. Run it on an isolated diagnostic worker in the initial PID namespace.
Enable the stock idle hook and the normal replay clock receipts. Start both
BPF collectors before the VMM and leave them active through session end.
Retain their stderr and shut down with SIGINT after the final observation.

```sh
sudo bpftrace -q -B line scripts/zb-demand/kvm-demand.bt \
  > demand.events 2> demand.stderr
python3 scripts/zb-demand/reconcile.py demand.events demand.stderr \
  replay.jsonl idle.jsonl > demand.json
```

The event log captures TDP handler entry and exit, guest address, GFN,
vCPU identity, HVA lookup time, host fault-path flags, and Linux task fault
counter deltas. It records no guest instructions or tool payload.
The join selects the restored VMM PID and pairs a TDP address with its GFN
and the matching captured KVM slot. The observed HVA must equal the slot
base plus the GPA offset. Repeated lookup attempts to the same HVA are
valid. Conflicting addresses, MMIO, hardware prefetch and missing lookups
remain visible as exclusions.

The output identifies faults for GPAs that were absent in VMM pagemap
at arm. Such a page can become resident in the interval before its demand.
A physical demand establishes an access sometime in the idle window even
if the access tracker could not arm that page. The VMM's KVM mapping
permissions can also cause TDP faults for an already resident page.
The guest kernel and other guest processes can issue accesses during tool
execution or a model wait. A tool dispatch aligns the timeline with the
host's tool interval without attributing each fault to the command.

Path evidence distinguishes a major filemap or swap path, other major
fault evidence, copy-on-write, and minor or unclassified MM faults.
Major flags and counter deltas come from nested MM calls or the vCPU
task while the HVA lookup runs. The scheduler observation brackets time
off-vCPU when the task enters an I/O wait during that lookup. It includes
any scheduling delay before the task runs again. Treat handler elapsed time,
memory lookup elapsed time, and observed off-vCPU I/O-wait time as nested
intervals. They do not establish incremental useful-work delay, block
device service time, bytes read, or the benefit of prior page-in.
There is no cumulative per-session stall total across vCPUs.

For tool-interval matches, the output gives time from dispatch and from the
simulated command availability receipt to the fault. The latter is emitted
before pre-tool tracing. The conservative elapsed time after subtracting the
largest possible tracing freeze union is also reported. The observed gap is
evidence of timing available to the replay harness. It does not establish
that the runtime could identify a particular missing page at that time.

## Calibrate address and path interpretation

Build and run the one-page calibration VM on the diagnostic worker while
the BPF listener is running. The VM uses the stock KVM API and a private
file that is unlinked after open. Its guest touches GPA zero and then writes
one byte in the same page. The program verifies the observed write and
outputs the PID, registered HVA, and pre-run cache residency from mincore.
The fixture verifies the host mapping has no present PTE before either run.

```sh
cc -Wall -Wextra -Werror -O2 -o /tmp/zb-demand-test-vm scripts/zb-demand/test_vm.c
sudo /tmp/zb-demand-test-vm warm > calibration-actions.jsonl
sudo /tmp/zb-demand-test-vm cold >> calibration-actions.jsonl
# Stop the calibration listener with SIGINT before checking its output.
python3 scripts/zb-demand/verify_calibration.py calibration-actions.jsonl \
  demand.events demand.stderr > calibration-summary.json
```

The cold run requests per-file cache discard and reports whether it worked.
For a valid calibration check that the first traced GPA and GFN are zero,
the HVA matches the fixture, and the warm and cold paths differ where
mincore shows different residency. A filemap major flag is evidence of a
cache-miss path. The scheduler and task-counter evidence describes the
specific fault independently of the cache state observed before the run.

Run offline joins with
`PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts/zb-demand`.
The trace also requires ready and clean end markers and rejects reported
BPF event loss.

## Trace the stock PyTorch block and DAX arms

Use the sealed `4b588047` cohort on the same idle native worker. Run
`check-torch-backing.py` against the completed block trace before starting
DAX. It compares the installed root and kernel bytes, live daemons, and
runtime profiles and records the worker boot identity.

```sh
sudo python3 scripts/zb-demand/check-torch-backing.py \
  amd201 /local/zb-texpilot fcmod-zepbound-tex /local/zb-torch-demand-4b58-01 \
  > deployment.json
sudo bash scripts/zb-demand/trace-torch-cohort.sh \
  SEALED_BUNDLE V4_RUNTIME V4_UNIT FRESH_OUTPUT PROBE_DIRECTORY v4
bash scripts/zb-demand/collect-torch-cohort.sh WORKER FRESH_OUTPUT RUNNER_OUTPUT v4
```

The probe directory contains the adjacent trace tools and
`scripts/zb-trace/vmm_exclusivity.py`. Keep `deployment.json` alongside the
collected files for reduction. The launcher resolves the balloon socket
after the restored VMM's first sample and records the read-only root pmem
configuration and guest counters. The KVM trace covers RAM and device slots.

[KVM SVM fault handling](https://github.com/torvalds/linux/blob/v6.8/arch/x86/kvm/svm/svm.c) ·
[KVM MMU page-fault construction](https://github.com/torvalds/linux/blob/v6.8/arch/x86/kvm/mmu/mmu_internal.h) ·
[Linux filemap fault path](https://github.com/torvalds/linux/blob/v6.8/mm/filemap.c)
