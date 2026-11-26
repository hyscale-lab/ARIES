# Crab calendar replay

From a prepared worker directory, run the normal Harbor CLI:

```sh
sudo ./aries-harbor -wildclaw fixture/trace.jsonl -wildclaw-mode crab -crab-arm checkpoint -crab-profile off -crab-run baseline-01
```

Use `-crab-arm control` to retain the sandbox through the five-second hold. Use `-crab-profile on` with a VM running the pinned DAMON reclaim and free-page reporting profile. A fresh recording name is required. Each invocation creates a new Crab sandbox and removes it after verification. The outer VM remains running.

The worker directory provides `qemu.pid` and an executable `./guest` which accepts one POSIX-shell command and forwards stdin/stdout to a dedicated Linux VM. Run the worker CLI with permission to read the VMM's smaps. QEMU's service cgroup, if present, is named `zb-crab-vmm.service`.

The guest needs a running stock Crab daemon on its default socket, guest sudo, and the SDK at `/opt/crab/venv/bin/python`. The tested upstream commit is `9607d61a41dc44358cf078c4b438bfd971c8ee9d`. Its default runtime root is `/var/lib/crab/runtime`. Use a dedicated local file-backed ZFS pool and the stock manual, non-lazy, non-incremental configuration with the interceptor disabled. Use a private VM with one vCPU and keep swap off. The adapter enforces the workload's 512 MiB limit and one CPU in its cgroup parent, which survives process restore.

Stage the pinned WildClawBench calendar workspace as Docker image `calendar-pilot:local` in the guest, with Python 3 and Git installed. The test requires the three published calendar inputs in a fresh master Git workspace under `/tmp_workspace`, and the agent user profile under `/root/USER.md`. Place the corresponding official verifier under `/var/tmp/fixture/tests` and reference output hashes in `/var/tmp/fixture/result-hashes.txt`. The loader requires the reviewed trace digest. The recorder uses the source clock and dispatches the opening three calls in parallel. It accepts the recorded Git exit 128.

The run directory contains transition times, SDK command return codes, replay receipts, token readbacks, verification scores, hashes, and host/guest memory samples. A checkpoint run also records the process image and ZFS snapshot sizes. Sampling includes the guest ARC and Crab helpers. Profile readbacks capture the guest kernel parameters, counters, page size, swap, and negotiated balloon feature bit.
