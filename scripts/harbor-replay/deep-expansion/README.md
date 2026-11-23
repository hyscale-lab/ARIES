# Expanded Harbor guest-memory replay

This entry point replays one of six reviewed originals on v1 or v4 with the wide-PFN guest collector, mapped ordinary-file identities, KVM slot and host page-presence evidence, and a fixed 8192-frame comparison against snapshot bytes. The worker driver records preflight and post-run inventory, one-second whole-worker placement, and the normal traced measurements. It keeps the original command and process boundaries and checks the source-specific final output.

The [Zepbound expansion results](https://github.com/hyscale-lab/Zepbound/tree/main/plan/result/1_opportunity/guest-memory-deep/expansion) contain the arm receipts, processed runs, input hashes and pair comparison. The Zepbound `proj/residency/expanded-deep/prepare-offline.py` recreates the offline regex and scheduling bundle from the archived source pairs, task receipts and original Ubuntu package archives.

Use a dedicated idle worker and a stock Firecracker 1.18.0 runtime. The retained shared diagnostic OS has SHA-256 `8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5`. It requests 2359296 PFNs and enables regular-file mapping identity. Use the sealed study tracer directory matching `trace-contract.py`. For a networked original, supply the reviewed private CNI directory with its live dependency checks. The worker script checks the deployed runtime and imports the supplied digest-pinned OCI image only when absent.

Build the pinned adapter with Go 1.26.8 in a clean checkout of `c0a6633e0bc726c35cfa447bf47524002d8d0439`.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
  go build -trimpath -buildvcs=false -mod=readonly -o ./aries-harbor-clock ./cmd/aries-harbor
sha256sum ./aries-harbor-clock
```

The expected executable SHA-256 is `557158ab56d79ee3f6fca0300f6b0dc423724d4b795adecb5d738a6cb5155541`. Stage and launch the exact source using a fresh private stage. Add `--cni "$cni"` for a networked source.

```sh
bash scripts/harbor-replay/deep-expansion/stage.sh "$stage" ./aries-harbor-clock
sudo -n bash "$stage/worker.sh" "$stage" "$trial" "$variant" \
  "$sealed_assets" "$sealed_tracer" "$runtime" "$unit" "$image"
```

Accept a successful arm only after the worker has recorded `worker.exit`, `idle-after.json`, and `result/fidelity-checks.json`, and the archived trace passes the Zepbound reducer. Archive the stage evidence before releasing the worker. To remove redundant launcher copies after the archive and idle checks, validate the recorded digests and remove only the named stage files.

```sh
sudo -n test -s "$stage/idle-after.json"
sudo -n sha256sum -c "$stage/launch-artifacts.sha256"
sudo -n rm -- "$stage/replay.py" "$stage/worker.sh" "$stage/trace-contract.py" \
  "$stage/audit-idle-runtime.py" "$stage/vmm_exclusivity.py" "$stage/aries-normal-launchclock-v3"
```
