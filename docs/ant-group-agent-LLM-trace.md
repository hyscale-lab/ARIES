# AntGroup Agentic LLM Trace 2026

## Introduction

This dataset captures a portion of the online serving workload from Ant Group's LLM inference infrastructure, collected during July 2026. The underlying data is sourced from Ant Group's production environment. The engine-side workload is served on open-sourced LLM instances running a large modern open-weight model. The harness side runs on a variant of the OpenClaw instance. Both engine-side and harness-side records are drawn from Ant's data service and filtered by instance name and LLM service.

The trace records requests served by an LLM engine along with the resource utilization of the virtual environments (pods) in which the agents execute. The data spans cache behavior (hit ratio and cached token length), request lifecycle metrics (TTFT, TPOT, prefill time, end-to-end latency, and queue length), engine state (batch size and context length), token statistics (input, output, and total token lengths), and pod-level CPU and memory utilization.

The harness-side records contain 24 hours of data, while the engine-side records capture requests from five individual pods over 24 hours each, and from a separate group of pods, randomly sampled from all serving pods during working hours (10:00-18:00). The working-hours pods are a different group and are not the same pods as those in the 24-hour records.

The dataset consists of this description and the following sets of files:

- LLM Engine request logs
- Harness environment metrics

## Files

The data files ship in the repository under `traces/` and are tracked with
[Git LFS](https://git-lfs.com/).

| File | Contents | Rows |
| --- | --- | --- |
| `antgroup_engine_trace_2026_24h_1.csv` | Engine request log, pod 1, 24 hours | 14,836 |
| `antgroup_engine_trace_2026_24h_2.csv` | Engine request log, pod 2, 24 hours | 12,257 |
| `antgroup_engine_trace_2026_24h_3.csv` | Engine request log, pod 3, 24 hours | 18,066 |
| `antgroup_engine_trace_2026_24h_4.csv` | Engine request log, pod 4, 24 hours | 7,580 |
| `antgroup_engine_trace_2026_24h_5.csv` | Engine request log, pod 5, 24 hours | 13,650 |
| `antgroup_engine_trace_2026_working_hours.csv` | Engine request log, randomly selected pods, working hours | 33,986 |
| `antgroup_harness_cpu_utilization_2026_24h.csv` | CPU utilization, 100 pods, 24 hours | 1,440 |
| `antgroup_harness_memory_utilization_2026_24h.csv` | Memory utilization, 100 pods, 24 hours | 1,440 |

Each `antgroup_engine_trace_2026_24h_*.csv` file records one pod's requests
over a 24-hour period; the pod identity is encoded in the file-name suffix
(`1`-`5`) rather than in a column. The working-hours file covers a different
group of pods randomly selected from all serving pods and includes a `pod`
column identifying each request's pod.

`antgroup_engine_trace_2026_working_hours.csv` is the engine-side file used in
the [ARIES paper](https://arxiv.org/abs/2607.29069).

## Using the Data

### License

The trace dataset is released under the
[Creative Commons Attribution 4.0 International (CC-BY-4.0)](../LICENSE) license.

### Downloading

The dataset is included in this repository under `traces/` and is stored with
Git LFS. After cloning the repository, fetch the trace files with:

```sh
git lfs pull
```

A separate standalone download location will be published at a later date.

### Schema

A complete field-level reference is available in [ant-group-agent-LLM-trace-schema.md](ant-group-agent-LLM-trace-schema.md).
