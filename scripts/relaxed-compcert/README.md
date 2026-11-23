# CompCert timing-tolerant measurements

The isolated adapter executes only the five digest-reviewed `compile-compcert` sources, with their original commands, networks, resource requests, concurrent call groups and modeled waits. It uses the normal native process start, yield and poll implementation. Each original first response retains its measured status and output. A late process is joined separately and an early terminal process retains its real exit code for subsequent recorded empty polls. Execution continues after a real terminal exit difference. The driver reports final compiler and dependency checks independently. Infrastructure errors terminate the affected run.

The patch applies to ARIES `de9649feef1fa0ae8e150d26890313cd71ead678`. To reproduce the executable, use Go 1.26.8 on Linux x86-64 and run `scripts/relaxed-compcert/build.sh /absolute/new/aries-relaxed-compcert`. The build runs the original Harbor/CLI tests and a foreground-process regression check, then outputs SHA-256 `b8228c9caae34f3adb073eb0cf3ac4c9c6bd50112e957fa03559c6ecd78efd5e`. Synthetic negative handle numbers are private host bookkeeping for a source-terminal command that yields live. A single guest process runs each source command.

The measured stage is sealed by `inputs.sha256` (SHA-256 `82ea256f5d171917976040de16501f52ff297e2743f8da90f98ecc2157b8d29c`). It contains the binary, the five ATIF and native rollout files, signed snapshot fixture, traced runner, and placement/network checks. To launch on a stock study worker with the source-appropriate tools and verified clean OS:

```bash
bash run-series.sh "$stage" "$variant" "$unit" "$runtime" "$cni_donor" "$codex_tools" \
    1d11 d116 dc2b bf3c 921a
```

The ordinary v1–v4 feature profiles use a 2-vCPU, 4-GiB guest and a 10-GiB workload disk. The driver verifies a fresh, exclusive guest for each run, records one-second PSS, KVM slot registrations, synchronous idle and resident page boundaries, native program output and all exit differences. The historical gVisor host's CPU model, PID allocation, random OPAM progress filenames and public dependency timing are unknown. The literal source continues to target those historical names in the two applicable recordings. Temporary source or output differences remain visible. The summary distinguishes an observed complete execution, its compiler verification and its level of agreement with the recorded replies.
