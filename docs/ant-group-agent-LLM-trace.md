# AntGroup Agentic LLM Trace 2026

## Introduction

This dataset captures a portion of the online serving workload from Ant Group's LLM inference infrastructure, collected during July 2026. The underlying data is sourced from Ant Group's production environment. The engine-side workload is served on open-sourced LLM instances running a large modern open-weight model. The harness side runs on a variant of the OpenClaw instance. Both engine-side and harness-side records are drawn from Ant's data service and filtered by instance name and LLM service.

The trace records requests served by an LLM engine along with the resource utilization of the virtual environments (pods) in which the agents execute. The data spans cache behavior (hit ratio and cached token length), request lifecycle metrics (TTFT, TPOT, prefill time, end-to-end latency, and queue length), engine state (batch size and context length), token statistics (input, output, and total token lengths), and pod-level CPU and memory utilization.

The harness-side records contain 24 hours of data, while the engine-side records capture requests from multiple serving instances during working hours.

The dataset consists of this description and the following sets of files:

- LLM Engine request logs
- Harness environment metrics

## Using the Data

### License

TBD

### Downloading

The dataset can be downloaded at: TBD

### Schema

A complete field-level reference is available in [ant-group-agent-LLM-trace-schema.md](ant-group-agent-LLM-trace-schema.md).
