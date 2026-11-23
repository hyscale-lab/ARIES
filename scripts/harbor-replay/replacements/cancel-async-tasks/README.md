# cancel-async-tasks replacement

This adapter accepts only the five reviewed ATIF and native source pairs for the
asyncio cancellation task. The source tools write `/app/run.py` and execute the
original Python concurrency and cancellation checks. Four pure heredoc patch
calls were intercepted by the original Codex shell frontend. The adapter uses
the bundled native patch helper with their exact patch bytes. The fifth source
keeps its initially missing `apply_patch` command and later executes its own
recorded shell function. Only the restored guest calls run. Recorded external
planning and web actions contribute their modeled waits.

The listed repository manifest contains Python 3.13.7. The force-built source
recordings showed 3.13.15. The pinned task Dockerfile uses
`FROM python:3.13-slim-bookworm` and `WORKDIR /app`. `image.py` verifies the
public 3.13.15 amd64 manifest and layers before building the corresponding
OCI archive. The available base was published after the recordings. Preserve
that chronology in the deployed image receipt.

Run the following from this directory with Go 1.26.8, the exact public source
files and the signed Debian package archive documented in the HF inspection
fixture. The builder takes the Git and GNU patch dependencies from that
verified archive. The image and adapter build require no live guest.

```sh
python3 -B image.py "$IMAGE"
CGO_ENABLED=0 python3 -B build.py "$STAGE" --row "$ROW" --sources "$SOURCES" \
  --image "$IMAGE" --package-archive "$DEBIAN_ARCHIVE"
cp worker.sh "$STAGE/"
cp -a "$TRACER" "$STAGE/tracer"
(cd "$STAGE" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS)
```

Use the [shared wide-PFN tracer contract](../../deep-expansion/trace-contract.py)
for both protocols. The ordinary worker entry takes a stage, variant and
`ordinary`. It imports the local immutable image into the verified study
namespace and runs each of the five sources in sequence. The `deep` entry
accepts v1 on amd201 and v4 on amd200. It uses the shared diagnostic guest root
and the median-duration source. No guest network is configured. Reserve a
worker before invoking either entry.

```sh
bash "$STAGE/worker.sh" "$STAGE" v1 ordinary
bash "$STAGE/worker.sh" "$STAGE" v1 deep
```

The original test results and final `run.py` digest are verified during
collection. Timing and stderr/stdout differences remain in the worker
receipts, with terminal mismatches and partial attempts retained as observed.
