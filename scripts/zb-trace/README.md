# Stock Firecracker idle tracing

Run the collector on the x86_64 worker in the host's initial PID namespace.
Use the stock Firecracker 1.18 binary, bpftrace 0.20 or newer, 4 KiB host
pages, and a kernel with KVM and page-idle support. Reading PFNs and marking
the idle bitmap requires privileges on the worker. Use one collector for a
worker's concurrent VMs, with a private output directory for each run.

Start slot capture before starting any Firecracker process intended for the
run. Leave the capture running through the last boundary. A successful start
prints a `ready` line. Give it at least one `/proc` clock tick before starting
the VM, so its truncated process start time is after the readiness marker.

```sh
sudo bpftrace -q -B line scripts/zb-trace/kvm-slots.bt > slot-events.txt
```

A replay driver calls `idle_hook.py PHASE RUNTIME_ID STEP SOCKET_PATH` with
`ZB_SLOT_EVENTS` set to that capture and `ZB_TRACE_OUT` to a new JSONL file
for the run. Use step zero for `session-start` and `session-end`, and the
same positive step ID for a command's `pre-tool` and `post-tool` boundaries.
Keep the JSONL file and slot capture alongside the replay event log. The
temporary armed state lives beside the JSONL file and is removed on the final
boundary. A run that fails before that boundary can leave an armed state file
to be removed with its temporary directory.

The hook identifies Firecracker using the API socket's peer credentials and
process start ticks. It accepts successful KVM ioctl registrations for that
process and checks their coverage against Firecracker's published x86_64
guest RAM layout. The recorded KVM FD, host address, guest address, and size
come directly from the registration. Registrations outside configured RAM
are recorded separately and excluded from idle-page sampling. Capture times
for slot registration are on the boot clock. Run, capture, and freeze times
are measured on the host monotonic clock.

The hook pauses the VM only around idle bitmap and page metadata observation.
It attempts to resume even on capture failure or a lost pause reply. A driver
that cancels a running hook must allow its SIGTERM handler time to resume the
VM before terminating the hook. A pause request and its acknowledgment
bracket the transition to paused. The resume request and its acknowledgment
or subsequent running-state check bracket the return to running. Frozen time
lies between the difference of the two interior timestamps and the difference
of the two exterior timestamps.

The first boundary arms idle marks. The pre-tool boundary collects the
preceding wait and rearms before command dispatch. The post-tool boundary
collects the command window and rearms for the next wait. The session end
collects the final span. Live-guest time spent between hook and dispatch,
including harness activity and background work, belongs to the corresponding
observation envelope. The driver event log gives the narrower command and
replay intervals.

For a separate intra-tool diagnostic, pass `-intra-tool-step 12
-intra-tool-interval 1s -intra-tool-windows 3` to the traced replay. The driver
waits for the real reply between captures, caps additional freezes at the
requested count, and records reply time in the execution goroutine. Each
`intra-tool` boundary collects and rearms the selected step. The last
`post-tool` boundary captures the tail. A live reply can race an already
started capture, so inspect each pause and reply timestamp. Summarize the
selected call with `python3 scripts/zb-trace/analyze_intra_tool.py RUN_DIR`.
Extra freezes perturb execution. Keep these receipts outside primary latency
comparisons and retain elapsed and pause-adjusted tool time.

Only stable, exclusive, resident host LRU base pages in the captured RAM
slots are classifiable. The log preserves newly resident pages, changed
mappings, and other unsupported cases as unknown. An accessed idle bit can
reflect guest background activity or VMM activity. It does not identify an
access direction or an exact command touch. The page-idle mechanism and
endpoint checks can miss some accesses and pages that appear and disappear
within a window. This hook does not classify guest objects, backing
recoverability, or filesystem page state.

Sources: [Firecracker x86_64 RAM layout](https://github.com/firecracker-microvm/firecracker/blob/v1.18.0/src/vmm/src/arch/x86_64/mod.rs), [KVM userspace memory regions](https://docs.kernel.org/virt/kvm/api.html#kvm-set-user-memory-region), [Linux idle page tracking](https://docs.kernel.org/admin-guide/mm/idle_page_tracking.html).

### Colocated original PyTorch sessions

Use `colocated_torch.py` on the idle amd201 block and DAX worker with the
sealed `4b588047` bundle and both task images already unpacked. Pass the bundle,
runtime base, fresh result directory, and v3 and v4 systemd unit names. The
launcher checks disk, RAM and thin-pool capacity, and verifies the installed
root bytes. It uses the Git 20-second baseline, 40-second stagger and 15-second
tail in v3, v4, v4, v3 order. The two roles have separate checkpoint and I/O
directories, CNI identities and writable backing. Each live readback verifies
that both restores use the same immutable root inode for that period.

Retain the launcher output in a sibling `.log` file and the exit code in a
sibling `.exit` file. `collect_colocated_torch.py` retrieves the raw receipts
and samples, reruns the sealed source verifier for every replay, and preserves
content-free host and VMM observations for the common colocation analysis.
