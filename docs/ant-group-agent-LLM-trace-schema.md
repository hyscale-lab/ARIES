# AntGroup Agentic LLM Trace Schema

This document describes the schema and file formats for the [AntGroup Agentic LLM Trace 2026](ant-group-agent-LLM-trace.md) dataset. It covers the LLM engine request logs and the harness environment metrics collected from Ant Group's production inference infrastructure.

## LLM Engine Request Trace

The data is collected from Ant Group's internal API platform, where both agentic and non-agentic workflows can be routed to the same instance. The instances are for internal use only. Since the API platform lacks a clear indicator of whether a harness is used, agentic and non-agentic requests are differentiated based on the input and response content.

The engine trace is provided as CSV files in two groups:

- **Working hours** - `antgroup_engine_trace_2026_working_hours.csv` covers requests during working hours from 10:00 to 18:00 from a group of pods randomly selected from all serving pods. It includes the `pod` field identifying each request's pod. This is the engine-side file used in the [ARIES paper](https://arxiv.org/abs/2607.29069).
- **24-hour per-pod logs** - `antgroup_engine_trace_2026_24h_1.csv` through `antgroup_engine_trace_2026_24h_5.csv` each record one pod's requests over a 24-hour period (five pods in total). They omit the `pod` field; the pod identity is encoded in the file-name suffix (`1`-`5`) rather than in a column.

> **Notes:**
> - The working-hours and 24-hour trace sets cover different pod groups and do not overlap.
> - The 24-hour files span a full calendar day, from 00:00 to 23:59.
> - The working-hours file comprises 10 pods in total.
> - Due to imperfections in the upstream data source, some data points may be missing; consumers should account for empty values when processing the data.

### Schema

| Field                 | Description                                                  |
| --------------------- | ------------------------------------------------------------ |
| pod                   | Label of the pod (virtual environment) that served the request; present only in `antgroup_engine_trace_2026_working_hours.csv` (the 24-hour files identify their pod by file-name suffix `1`-`5`) |
| emit_time             | Timestamp of the log record, expressed as a relative time    |
| actual_hit_ratio      | Actual prefix-cache hit ratio of the request                 |
| check_pass            | Whether the request completed successfully                   |
| chunk_count           | Number of chunks processed for the request                   |
| duration              | Total processing time of the request                         |
| engine_batch_size     | Engine batch size at the time the request was processed      |
| engine_context_length | Engine context size at the time the request was processed    |
| engine_e2e            | End-to-end latency measured on the engine side               |
| engine_prefill_time   | Time spent by the engine in the prefill phase                |
| engine_queue          | Engine queue length at the time the request was processed    |
| engine_ttft           | Time to first token (TTFT)                                   |
| tpot                  | Time per output token (TPOT)                                 |
| input_text_length     | Length of the input text                                     |
| input_token_length    | Number of input tokens                                       |
| output_token_length   | Number of output tokens                                      |
| request_id            | Unique ID of the request                                     |
| has_tool_calls        | Whether the request involved tool calls <sup>1</sup>         |

1. `has_tool_calls` is a generated column derived from the input and response content of the original data. It is set to `true` when the content contains agentic tool call headers (e.g., `tool_call_start`, `tool_call_end`). It serves as the primary indicator of agentic application, as the raw data does not include such an indicator.

## Harness Environment Metrics

The harness side is a variant of OpenClaw used internally for tasks such as office assistance, meeting room booking, querying internal materials, tracking workflow progress, and creating personal plans. Metrics are collected from an internal monitoring platform.

> **Note:** The engine-side and harness-side data are not from the same period and production environment, so they are not directly relatable.

CPU and memory utilization for 100 pods, collected over a 24-hour period, are provided as CSV files: `antgroup_harness_cpu_utilization_2026_24h.csv` and `antgroup_harness_memory_utilization_2026_24h.csv`

Each file contains 100 columns, one per pod. Columns follow the pattern `[Metric](pod=[pod name])`, where `Metric` is `cpu_util` or `mem_util`.

> **Notes:**
> - Utilization values are expressed as percentages of allocation (%).
> - Harness pods are not necessarily active for the full 24-hour period; utilization is recorded only over each pod's lifespan.
