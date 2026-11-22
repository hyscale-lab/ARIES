# Sandbox backends

Whether other tool-sandbox backends can serve ARIES, and what they cost to
start, run and remove. The candidates are:

- [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox), with the
  runc, gVisor and Kata runtimes, and its warm pools;
- [CubeSandbox](https://github.com/tencentcloud/CubeSandbox);
- [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox).

Each is compared against the existing Docker and Kubernetes pod sandboxes.
Only the tool sandbox changes; the harnesses, the bridge, the replayed model
and the tasks are the same as in [`coldwarm`](../coldwarm/README.md).

[`report.md`](report.md) holds the findings and the code changes so far.

| Path | What it holds |
| --- | --- |
| `report.md` | Set-up, results, findings, code changes and open items |
| `phase0/` | Probes that drive each backend directly, without ARIES, and their results |
| `phase1/tasks.tsv` | One row per task run of the ARIES integration replays |
| `figures/` | The report's figures, PNG and PDF, drawn from `phase0/` and `phase1/` by `scripts/microbench/plot_sandbox.py` |
| `experiment.json`, `results/` | The cold and warm campaign over the Kubernetes setups, run like [`coldwarm`](../coldwarm/README.md) |

Phase 0 runs `phase0/probe.py BACKEND checks` and
`phase0/probe.py BACKEND latency --iterations 50 --concurrency C` on a worker;
`phase0/run-latency.sh` drives the whole latency sweep. `phase0/probe_api.py`
holds the CubeSandbox and OpenSandbox adapters.

Phase 1 replays the [20261115-e5-2630v3](../coldwarm/results/20261115-e5-2630v3/)
recordings through `scripts/microbench/run.sh` with `--runtime-class`,
`--agent-sandbox` and `--warm-pool`. The raw runs are not kept in Git.

The campaign replays the coldwarm recordings on seven setups: the plain pod,
agent-sandbox under runc, gVisor and Kata, and a warm pool of one under each
runtime. Its Kubernetes runs use a bridge image loaded onto the workers
(`bridge_repository`), built from the campaign's commit.

```sh
python3 scripts/experiment/run.py start experiments/sandbox --campaign <date>-<hardware> \
  --recordings-from experiments/coldwarm/results/20261115-e5-2630v3
```
