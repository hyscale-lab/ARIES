# Experiments

An **experiment** is a measurement defined once and run many times. A
**campaign** is one run of it on one cluster. Each campaign keeps its
provenance, its runs, its analysis and its report together.

```text
experiments/<experiment>/              one experiment
  experiment.json                      what to measure: harnesses, setups, tasks, repetitions, warm
  README.md                            purpose and how to run it
  results/<campaign>/                  one execution
    meta.json                          commits, bridge image, nodes and hardware, recordings, times
    experiment.json                    the spec as it was run
    manifest.tsv                       every run, with status and reason
    marks.tsv                          status changes made by hand (run.py mark)
    notes.md                           interpretation, written by hand
    report.md                          generated, high level: setup, stages, figures, results, overhead, notes
    tasks/<task>.md                    generated, fine grained: every phase, overhead, tool calls, resources
    figures/                           generated PNG, PDF and figures.csv
    figures/tasks/                     generated per-task figures, shown in tasks/<task>.md
    analysis/                          generated tasks.csv and analysis.md
    runs/<harness>/<setup>/<rep>/      raw runs with their request logs (ignored)
    recordings/                        model recordings and their decoded reports (ignored)
    logs/                              chain scripts, chain and step logs, worker manifests (ignored)
    state.json                         SSH targets for this campaign (ignored)
```

Git tracks the spec, `meta.json`, the manifest, the marks, the notes, the
generated report, the figures and the analysis. Raw runs, recordings, logs
and SSH targets stay local, because they hold full agent trajectories and
node addresses. The report can always be regenerated from a local copy of the
raw runs.

## The spec

`experiment.json` takes these keys. Any other key is rejected.

| Key | Meaning |
|---|---|
| `name` | The experiment's name. On the workers, campaigns run in `~/aries-campaigns/<name>/`. |
| `description` | One line saying what is measured. It opens the report's Setup section. |
| `harnesses` | The harnesses to measure, `openclaw` and `hermes`. Each gets a worker of its own; with more harnesses than workers, a worker runs them one after another. |
| `setups` | Where tools run: `docker` (an ARIES Docker sandbox), `k8s` (an ARIES Kubernetes pod) and `native` (the harness's own Docker sandbox). Each repetition runs them in this order. `native` must come after `docker`, because the native baseline reuses an ARIES Docker run's harness configuration. |
| `tasks` | The microbenchmark tasks each run includes, from `benchmarks/microbench/`. |
| `repetitions` | How many times each setup runs. |
| `warm` | `true` runs each task cold on a fresh harness and sandbox, then warm on the same ones, so every run gives both measurements. `false` runs it cold only. |
| `record_warm` | `true` records the warm run separately from the cold one, so warm replays get the warm run's own replies. `false` records the cold run only. Defaults to `warm`. Skipped with `--recordings-from`. |
| `bridge_repository` | Empty: Kubernetes runs pull the bridge image from the repository the profiles name. Set it to use another, such as an image loaded onto the nodes. The tag stays this checkout's bridge revision. |

Two choices are fixed rather than keys. Harnesses always start alongside
their sandboxes, as ARIES does by default. Recordings are always made on
ARIES Docker.

## Running a campaign

You need a cluster built by `k8s/create-cluster.sh` and described in
`k8s/cluster.json`, a committed working tree, and, unless you pass
`--recordings-from`, `DEEPSEEK_API.key` for the recording step.

```sh
python3 scripts/experiment/run.py start experiments/coldwarm --dry-run
python3 scripts/experiment/run.py start experiments/coldwarm --campaign 20261115-e5-2630v3
python3 scripts/experiment/run.py status experiments/coldwarm/results/20261115-e5-2630v3
python3 scripts/experiment/run.py collect experiments/coldwarm/results/20261115-e5-2630v3 --watch 300
python3 scripts/experiment/report.py experiments/coldwarm/results/20261115-e5-2630v3
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
setup alike. Before the first measured run, each setup gets one warm-up run of the first
task. It pulls every image the setup uses, into Docker and into the node's
containerd, so no measured run pays for a pull. Warm-up runs are kept but
listed as superseded. Each step is one `scripts/microbench/run.sh` or
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
  `resume --prepare` ships and builds the tree again first. Add `--worker <name>`
  to touch only that worker, leaving the others' running chains alone. A
  re-prepared worker's commit is kept in `meta.json` and shown in the report.
- **stop** terminates each chain's process group. ARIES cleans its
  sandboxes up on SIGTERM. The native baselines do not, so check for leftover
  containers after stopping one. Anything still running 90 seconds later is
  killed. The interrupted step has no result, and resume runs it again.
- **rerun** supersedes a measured run, with a reason, and has its worker run
  that step again once its chain has finished:

  ```sh
  python3 scripts/experiment/run.py rerun experiments/coldwarm/results/20261115-e5-2630v3 \
    runs/hermes/k8s/1 --reason "first Kubernetes pod on the node pulled the harness image"
  ```

- **mark** records a judgement made by hand. The run stays on disk, but only
  valid runs are analysed. The mark is kept in `marks.tsv` and survives later
  collects:

  ```sh
  python3 scripts/experiment/run.py mark experiments/coldwarm/results/20261115-e5-2630v3 \
    runs/hermes/k8s/2 --status superseded --reason "cAdvisor pod restarted mid-run"
  ```

The report lists every run left out, with its reason.

## The report

`report.py` writes a manifest of the valid runs in `analysis/`, with paths
relative to the campaign, and runs `scripts/microbench/analyze.py` over it.
For warm experiments, it then draws the figures with
`scripts/microbench/plot_coldwarm.py` and decodes each recording with
`scripts/microbench/decode_recordings.py`.

`report.md` is the high-level report, laid out like the original summary:

- the setup: setups, controls (hardware, resources, model replay, resource
  measurement) and tasks;
- the stages of a task;
- the figures;
- the results: cold and warm latency per task, and each ARIES setup's
  overhead against native (start-up, cold and warm latency, cost per tool
  call, warm harness logic);
- links to the per-task reports, the run counts and the runs left out;
- the provenance, from `meta.json`;
- `notes.md`, copied in verbatim. Write interpretation there, with headings
  from `###` down, never in `report.md`.

`tasks/<task>.md` holds one task's fine-grained metrics for each harness, as
mean ± standard deviation with each ARIES setup's difference from native.
It opens with figures from `figures/tasks/`:

- the task's cold and warm stages, with what follows the answer;
- harness CPU, sandbox CPU and harness memory over time for each setup's run
  with the median cold latency, timed from the start of the cold run. These are
  drawn from the raw runs' `monitor/resources.jsonl`, so they need a local
  copy of the runs.

Then:

- every start-up phase;
- the cold and warm runs split into model, tool execution and harness logic;
- the harness's overhead (its start and logic);
- tool calls: the agent's calls, the commands the bridge ran for them, and
  per call the harness-timed cost, the bridge's share and the transport
  between them;
- what follows the answer;
- CPU and memory;
- the cold and warm latency of every repetition.

Rerun `report.py` after every `collect` or `mark`.

## Reusing recordings

A campaign records once per harness. To hold the model constant across
campaigns, for example on new hardware, replay an earlier campaign's
recordings:

```sh
python3 scripts/experiment/run.py start experiments/coldwarm \
  --recordings-from experiments/coldwarm/results/20261102-e5-2640v4
```

`meta.json` keeps each recording's SHA-256, so the reports show whether two
campaigns replayed the same model output.

## Existing results

- [`experiments/coldwarm/results/20261102-e5-2640v4`](../experiments/coldwarm/results/20261102-e5-2640v4/report.md):
  the first cold and warm campaign (Xeon E5-2640 v4, Kubernetes 1.34 on an
  Invitro-built cluster). It was migrated into this layout after the fact, so
  its `meta.json` was reconstructed and names the commits of each part.
- [`experiments/microbench/results/legacy`](../experiments/microbench/results/legacy/report.md): the
  first comparison, written by hand before this layout existed. Only its
  report and figures are kept.

The lower-level tools are described in
[configuration: microbenchmark profiles](configuration.md#microbenchmark-profiles).
