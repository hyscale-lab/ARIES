# Experiments

An **experiment** is a measurement defined once and run many times. A
**campaign** is one run of it on one cluster. Each campaign keeps its
provenance, its runs, its analysis and its report together.

```text
experiments/<experiment>/          what to measure (committed)
  experiment.json                  harnesses, setups, tasks, repetitions, warm
  README.md                        purpose and how to run it
results/<experiment>/<campaign>/   one execution
  meta.json                        commits, bridge image, nodes and hardware, recordings, times
  experiment.json                  the spec as it was run
  manifest.tsv                     every run, with status and reason
  marks.tsv                        status changes made by hand (run.py mark)
  notes.md                         interpretation, written by hand
  report.md                        generated: setup, counts, figures, stage tables, notes
  figures/                         generated PNG, PDF and figures.csv
  analysis/                        generated tasks.csv and analysis.md
  runs/<harness>/<setup>/<rep>/    raw runs with their request logs (ignored)
  recordings/                      model recordings and their decoded reports (ignored)
  logs/                            chain scripts, chain and step logs, worker manifests (ignored)
  state.json                       SSH targets for this campaign (ignored)
```

Git tracks the spec, `meta.json`, the manifest, the marks, the notes, the
generated report, the figures and the analysis. Raw runs, recordings, logs
and SSH targets stay local, because they hold full agent trajectories and
node addresses. The report can always be regenerated from a local copy of the
raw runs.

## Running a campaign

You need a cluster built by `k8s/create-cluster.sh` and described in
`k8s/cluster.json`, a committed working tree, and, unless you pass
`--recordings-from`, `DEEPSEEK_API.key` for the recording step.

```sh
python3 scripts/experiment/run.py start experiments/coldwarm --dry-run
python3 scripts/experiment/run.py start experiments/coldwarm --campaign 20261115-e5-2630v3
python3 scripts/experiment/run.py status results/coldwarm/20261115-e5-2630v3
python3 scripts/experiment/run.py collect results/coldwarm/20261115-e5-2630v3 --watch 300
python3 scripts/experiment/report.py results/coldwarm/20261115-e5-2630v3
```

`start` does the following:

1. Packs the microbenchmark tasks here (packing needs Git history, which the
   workers' copy lacks). Then ships them and `git archive HEAD` to each
   worker that will run a harness, and runs `make build` there, stamped with
   this checkout's bridge revision.
2. Copies the cluster's kubeconfig and, when the campaign must record, the
   model key. Both go through a pipe and are written with mode 600.
3. Reads each worker's Kubernetes node name and internal address, and the
   kube-prometheus-stack Service address.
4. Records the hardware facts in `meta.json`.
5. Starts one background chain per worker.

Each harness gets a worker of its own, and all setups of a harness run on that
worker, so setups are compared on the same hardware.

A chain installs the campaign's recording, or records one first (moving any
older recording on the node aside). It then runs every repetition, cycling
through the setups in the order the spec lists them, so drift affects every
setup alike. Each step is one `scripts/microbench/run.sh` or
`scripts/baseline/<harness>_native.py` invocation with `--runs 1`. The chain
copies the step's run and its model request log to
`runs/<harness>/<setup>/<rep>/` and lists the run in its manifest:

- **valid**: the run finished with a `run-result.json` and ARIES exited 0.
- **failed**: anything else, with the reason.

A failed step does not stop the chain. A failed recording skips that
harness's runs.

`native` must come after `docker` in `setups`: the native baselines take
their harness configuration from an ARIES Docker replay.

## Failures, retries and exclusions

- **resume** restarts any chain that has stopped. Finished steps are skipped,
  and failed or interrupted steps run again. A retry is stored beside the
  first attempt, for example as `runs/openclaw/k8s/3-2`. When the retry is
  valid, `collect` marks the earlier attempt **superseded**.
  `resume --prepare` ships and builds the tree again first.
- **stop** terminates each chain's process group. ARIES cleans its
  sandboxes up on SIGTERM. The native baselines do not, so check for leftover
  containers after stopping one. Anything still running 90 seconds later is
  killed. The interrupted step has no result, and resume runs it again.
- **mark** records a judgement made by hand. The run stays on disk, but only
  valid runs are analysed. The mark is kept in `marks.tsv` and survives later
  collects:

  ```sh
  python3 scripts/experiment/run.py mark results/coldwarm/20261115-e5-2630v3 \
    runs/hermes/k8s/2 --status superseded --reason "cAdvisor pod restarted mid-run"
  ```

The report lists every run left out, with its reason.

## The report

`report.py` writes a manifest of the valid runs in `analysis/`, with paths
relative to the campaign, and runs `scripts/microbench/analyze.py` over it.
For warm experiments, it then draws the figures with
`scripts/microbench/plot_coldwarm.py` and decodes each recording with
`scripts/microbench/decode_recordings.py`.

`report.md` covers:

- the setup and provenance, from `meta.json`;
- run counts by status, and the runs left out;
- the figures;
- the per-task stage tables, the start-up table and the after-answer table,
  computed from `analysis/tasks.csv`;
- `notes.md`, copied in verbatim. Write interpretation there, with headings
  from `###` down, never in `report.md`.

Rerun `report.py` after every `collect` or `mark`.

## Reusing recordings

A campaign records once per harness. To hold the model constant across
campaigns, for example on new hardware, replay an earlier campaign's
recordings:

```sh
python3 scripts/experiment/run.py start experiments/coldwarm \
  --recordings-from results/coldwarm/20261102-e5-2640v4
```

`meta.json` keeps each recording's SHA-256, so the reports show whether two
campaigns replayed the same model output.

## Existing results

- [`results/coldwarm/20261102-e5-2640v4`](../results/coldwarm/20261102-e5-2640v4/report.md):
  the first cold and warm campaign (Xeon E5-2640 v4, Kubernetes 1.34 on an
  Invitro-built cluster). It was migrated into this layout after the fact, so
  its `meta.json` was reconstructed and names the commits of each part.
- [`results/microbench/legacy`](../results/microbench/legacy/report.md): the
  first comparison, written by hand before this layout existed. Only its
  report and figures are kept.

The lower-level tools are described in
[configuration: microbenchmark profiles](configuration.md#microbenchmark-profiles).
