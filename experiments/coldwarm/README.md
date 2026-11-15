# Cold and warm start

What a user waits for when an agent's tools run natively, in an ARIES Docker
sandbox, or in an ARIES Kubernetes pod, for OpenClaw and Hermes.

- **Cold:** the first request on a fresh harness and sandbox, from the first
  start-up step to the answer.
- **Warm:** the same task again on the running harness and sandbox.

Each run is split into start-up, harness start, model (replayed), tool
execution and harness logic. The model is held constant by recording each
harness's replies once and replaying them in every run.

`experiment.json` sets the harnesses, setups, the five microbenchmark tasks
(`benchmarks/microbench/`) and the repetitions. With `warm`, every run takes
both measurements. Each harness and the sandbox get 2 CPUs and 4 GiB
(`profiles/comparison/`).

```sh
python3 scripts/experiment/run.py start experiments/coldwarm --campaign <date>-<hardware>
python3 scripts/experiment/run.py collect experiments/coldwarm/results/<campaign> --watch 300
python3 scripts/experiment/report.py experiments/coldwarm/results/<campaign>
```

On a cluster with two workers, the two harnesses run in parallel. Each takes
about two hours for 5 repetitions. See [docs/experiments.md](../../docs/experiments.md)
for the campaign layout, resuming and marking runs. Campaigns are in
[`results/`](results/).
