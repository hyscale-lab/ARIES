# Guest scheduling during the original TeX compilation

This isolated diagnostic replays the recorded `overfull-hbox` trial
`337f0b12-666b-4f8f-bd8e-1eec7ab2de7f` through the native File checkpoint
path on study worker amd200. It uses the recorded step 12 Python command with
its real patch helpers and repeated `pdflatex` launches. The guest service
starts with the diagnostic root and accepts fixed arm, mark and stop requests
over its separate host-only vsock port. It changes no workload command,
input, file-cache policy, or primary deployment.

`observe.py` arms one private tracefs instance before dispatch and stops it
after the reply. It captures scheduler switches, waking and wakeup, exec/fork/exit
lineage, regular file-cache insertions and block BIO queue/completion events. A
stack trigger takes kernel stacks for uninterruptible switch-outs. The service
records five bracketed `mono` markers on either side, CPU-buffer loss counters,
tracepoint field formats and guest mount information. The returned trace is a
bounded gzip payload. The separate host probes record KVM host-PFN lookups and
memslot registration. Their timestamps use the host clocks.

## Regenerate the staged fixture

Use local study inputs and Go 1.26.8. The trace-enabled guest kernel and config
come from the [pinned event-tracing build](../zb-wheel-filemap/build-kernel.sh).
They retain the vendor revision `7483450dec64b4f923037de31d8c28298a7c3359`.
The original deployment kernel and the trace-enabled build have different
configurations. The separate application OCI image is unchanged.

```bash
scripts/zb-tex-sched/build-root.sh "$CLEAN_ROOT" "$FIXTURE"
mkdir "$STAGE"
cp "$FIXTURE/"{diagnostic.ext4.gz,root.sha256} "$STAGE/"
cp "$TRACING_KERNEL/"{tracing.vmlinux,tracing.config} "$STAGE/"
cp "$CLOCK_ADAPTER" "$STAGE/clock-adapter"
cp scripts/zb-tex-sched/{run-worker.sh,observe.py,trace-host.sh} "$STAGE/"
cp scripts/zb-demand/kvm-demand.bt scripts/zb-trace/{provision_baseline.sh,vmm_exclusivity.py} \
   scripts/harbor-replay/audit-idle-runtime.py "$STAGE/"
(cd "$STAGE" && sha256sum -b -- * > SHA256SUMS)
```

The historical input adapter is `c0a6633` SHA-256
`557158ab56d79ee3f6fca0300f6b0dc423724d4b795adecb5d738a6cb5155541`.
The trace-enabled kernel SHA-256 is
`6b0bf116f7baad32304742d9dc43e97f6541635feee6f16377cab817e0e563a1`.
The worker entry point expects the verified stock native v4 donor at
`/local/zb-filemaps-v4/runtime` and the original assets at
`/local/zb-v2/tex-pilot`. Copy to an empty stage on the reserved worker and
run `bash "$STAGE/run-worker.sh" "$STAGE"` as the study owner with the
existing worker privileges. Preserve the stage inventory and worker raw
trace files before dismantling the isolated unit and pool.
