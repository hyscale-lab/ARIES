# Full DeltaBox calendar pilot

QEMU keeps running throughout the hold and restore. The authors’ startup check exercises synchronous CRIU and DeltaFS. The calendar pilot uses the stock SandboxDriver warm-template profile. It releases the active subtree and retains the stopped fork template, controller, helper processes and DeltaFS layers in the guest.

## Staging pc557

Use the worker's verified local disk. The campaign used /local/zb-cal-full on /dev/sda3 (ext3). Confirm the mount table, capacity, reservations, SSH login home backing, and KVM access first. Run QEMU and image setup with the node's existing passwordless sudo. The packages below are unpacked into the pilot directory.

1. Create local tmp, images, apt/lists/partial, apt/archives/partial, and apt/root directories. Clone the DeltaBox artifact into ae at revision 1054de552758229f34daebd34f696da0d56e7919. Verify ae/linux/vmlinux has SHA-256 0ba5e9ee744bcb2bac3391e967cf36cfeb10fd3add067055321cb136cd5004c0.
2. Run the pinned downloader with python3 ae/ae/appliance/download_appliance.py --output-dir images --jobs 12 --extract. Its checksum-verified output is DeltaBox-ATC2026-v3-l1.qcow2, SHA-256 5b7455ebaa2d5bec7380642c1aa25147a55847252bbbcecfdb89882aa2801191. The v4 layer URL returned 404 on pc557. The preparation script reconstructs it on the pinned base in an isolated mount namespace through a QEMU FUSE export.
3. Populate local APT lists using apt-get -o Dir::State::lists="$PWD/apt/lists" -o Dir::Cache="$PWD/apt" -o APT::Sandbox::User="$(id -un)" update. Use the same options plus -o Debug::NoLocking=true and -y --download-only --no-install-recommends install qemu-system-x86 qemu-utils. Run dpkg-deb -x on each downloaded apt/archives/*.deb with apt/root as the destination. Download util-linux-extra and unpack it into apt/fincore for the per-file cache check.
4. Copy host.py and prepare-images.sh to the worker pilot directory. Put guest.py, worker.py, the 477-byte /root/USER.md template, and the three task inputs under environment/ in payload/. Put the seven original verifier files in a separate verifier/ tree. Copy the pinned session as trace.jsonl and the expected hashes as result-hashes.txt into the pilot directory. The input and verifier digests are in the [pinned Harbor task](https://huggingface.co/datasets/InternLM/WildClawBench-Harbor/tree/c880033a1bd4023ffe0175bb2983b46719a7e36b/01_Productivity_Flow_task_6_calendar_scheduling). The template's digest is 178a5807bd2e6ace25b80a366f0f7d8d3f354a47f577c4ab0092397456b8738f.
5. From the isolated directory, run sudo -n unshare -m --propagation private bash ./prepare-images.sh. It reconstructs the authors' direct check layer and a separate calendar layer. The two layers share the verified read-only base. The immutable appliance, fixture, and verifier drives remain read-only. The helper stages the stock driver and controller at the same published source revision as the kernel.

## Pilot

Run the stock coupled check first, with the QEMU binaries and library directory from the unpacked packages in PATH and LD_LIBRARY_PATH. Pass the equivalent of -L "$PWD/apt/root/usr/share/qemu" via a scratch QEMU wrapper so the stock launcher can find its firmware. The pinned command is python3 ae/ae/appliance/deltabox.py check --images images --kernel ae/linux/vmlinux --log runs/coupled-serial.log.

Each calendar invocation starts a fresh offline guest. Use one vCPU and 512 MiB for the complete VM. This appliance kernel lacks the memory and CPU cgroup quota controllers. The runtime-default profile holds a fork template and restores from it. The first checkpoint has no incremental parent. Filesystem rollback is checked with a saved and post-capture Git metadata marker outside the trace's workspace listing. Verification starts after the harness closes the tool bridge and releases the worker. It mounts the dedicated verifier disk in the controller namespace and scores the same workspace.

```bash
sudo -n env TMPDIR="$PWD/tmp" python3 ./host.py checkpoint trace.jsonl result-hashes.txt >checkpoint.jsonl
sudo -n env TMPDIR="$PWD/tmp" python3 ./host.py control trace.jsonl result-hashes.txt >control.jsonl
python3 summarize.py checkpoint.jsonl control.jsonl
```

The baseline guest verifies that DAMON parameters and a virtio balloon are absent and swap is disabled. The AgentENV treatment requires DAMON, which is disabled in this kernel build.

The host samples QEMU smaps_rollup and records its PID and CPU ticks. Host PSS includes shared, anonymous and file-backed mappings. The launcher evicts only the experiment input files from the host page cache and records their remaining residency with fincore before every arm. Kernel file cache outside QEMU mappings is outside PSS. The in-guest observer collects proportional process totals, including the surviving controller and any helpers, and guest Cached and Shmem totals. Checkpoint logical and allocated tmpfs bytes are reported separately from on-disk bytes. Process release is acknowledged once the active worker's original host-namespace PID is absent and the retained template is stopped. The hold contains no workload RPC. The restored worker must return the original random token from memory, report its fixed in-namespace PID 100, and have a new host-namespace PID.


## Run through ARIES

Copy serve.py alongside host.py in the prepared directory. Build cmd/aries-harbor
from this branch. Run each arm on the same idle worker and save the ARIES
scheduler and the appliance receipts separately.

    sudo -n env TMPDIR="$PWD/tmp" ./aries-harbor -wildclaw-calendar trace.jsonl -calendar-deltabox "$PWD" -calendar-deltabox-arm checkpoint > checkpoint-replay.jsonl 2> checkpoint-host.jsonl
    sudo -n env TMPDIR="$PWD/tmp" ./aries-harbor -wildclaw-calendar trace.jsonl -calendar-deltabox "$PWD" -calendar-deltabox-arm control > control-replay.jsonl 2> control-host.jsonl
    python3 summarize.py checkpoint-host.jsonl control-host.jsonl

ARIES verifies the source digest, runs the lifecycle, preserves the original
waits through its native calendar scheduler, dispatches the opening batch
together, and checks the three output hashes after the official grader passes.
