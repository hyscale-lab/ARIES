# Bayesian-network replay

The adapter accepts five exact Codex/Harbor recordings of `bn-fit-modify` on the pinned Linux amd64 image. Each original uses base R, Python, and the source-qualified Codex tools in the guest. One host-side documentation search per source contributes its recorded interval. The guest has no network. [Task](https://hub.harborframework.com/tasks/terminal-bench/bn-fit-modify/latest)

Run the adapter build with Go 1.26.8 and `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1`. `stage.py` takes the exact source-pair directory, archived five-row Harbor index, adapter binary, study tracer directory, and new bundle directory. It compiles each source timeline, verifies the tracer and source hashes, and selects the median source by original job duration and UUID. The resulting `SHA256SUMS` seals the bundle.

`submit.sh WORKER SEALED_BUNDLE RAW_OUTPUT` starts the private worker controller for the worker-qualified native profile. Each lane prepares, snapshots, restores and measures one VM at a time. A fresh archive is acknowledged before the next source starts. The runner keeps one `*-worker.tar.gz` per arm and a terminal queue per worker. Deep v1 and v4 run serially on amd201 with the wide-PFN root and identical sample frame selection.

After the measured session, `bn_fit_artifacts` captures the three actual output CSVs and the unchanged-input hash. Each captured output includes its SHA-256, size and gzip+base64 bytes. The benchmark verifier runs outside the measured VM on those bytes. First native replies, actual status differences, deferred native completions, and the after-session evidence remain separately visible.

After the corresponding measurements are published, `retire.py WORKER RAW_OUTPUT --remove` verifies the published archive digests and launch scripts, checks the worker is idle, and removes three redundant worker-side wrappers. The input and archive files remain available.
