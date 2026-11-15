Means over replayed runs; times in seconds, CPU in percent of one core.

### hermes, mb-cpu, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 19.76 | 19.72 | 21.97 | +2.24 |
| cold_startup_s | 8.10 | 8.14 | 10.68 | +2.54 |
| provisioning_s | 0.00 | 0.26 | 2.19 | +1.93 |
| harness_prepare_s |  | 0.02 | 1.52 | +1.50 |
| harness_start_s | 8.10 | 8.12 | 8.95 | +0.83 |
| cold_run_s | 11.66 | 11.58 | 11.29 | -0.29 |
| cold_model_s | 2.34 | 2.34 | 2.34 | -0.00 |
| cold_tool_exec_s | 3.89 | 4.18 | 4.43 | +0.25 |
| cold_harness_logic_s | 5.43 | 5.07 | 4.52 | -0.54 |
| cold_tool_s | 0.00 | 4.02 | 4.62 | +0.59 |
| warm_latency_s | 5.99 | 6.27 | 6.48 | +0.21 |
| warm_model_s | 2.34 | 2.34 | 2.34 | -0.00 |
| warm_tool_exec_s | 3.38 | 3.69 | 3.90 | +0.21 |
| warm_harness_logic_s | 0.28 | 0.24 | 0.24 | -0.00 |
| warm_tool_s | 0.00 | 3.44 | 3.67 | +0.23 |
| agent_internal_s |  |  |  |  |
| task_s | 26.40 | 28.68 | 36.00 | +7.32 |
| harness_collect_s |  | 2.04 | 2.23 | +0.19 |
| isolation_s |  | 0.37 | 1.39 | +1.02 |
| evaluation_s | 0.23 | 0.16 | 0.87 | +0.71 |
| cleanup_s | 0.26 | 0.11 | 3.06 | +2.95 |
| cold_tool_mean_ms |  | 143.74 | 164.85 | +21.11 |
| warm_tool_mean_ms |  | 245.83 | 262.40 | +16.57 |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 27.44 | 26.73 | 21.85 | -4.88 |
| sandbox_cpu_peak | 100.08 | 100.10 | 100.06 | -0.04 |
| harness_cpu_peak | 155.62 | 158.26 | 156.41 | -1.85 |
| harness_memory_peak_mib | 256.66 | 261.71 | 256.82 | -4.89 |

### hermes, mb-noop, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 14.65 | 14.35 | 16.40 | +2.06 |
| cold_startup_s | 8.05 | 8.03 | 10.67 | +2.64 |
| provisioning_s | 0.00 | 0.26 | 2.20 | +1.94 |
| harness_prepare_s |  | 0.02 | 1.52 | +1.50 |
| harness_start_s | 8.05 | 8.01 | 8.94 | +0.93 |
| cold_run_s | 6.60 | 6.31 | 5.73 | -0.58 |
| cold_model_s | 1.21 | 1.21 | 1.21 | -0.00 |
| cold_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| cold_harness_logic_s | 5.39 | 5.10 | 4.52 | -0.58 |
| cold_tool_s | 0.00 | 0.21 | 0.45 | +0.24 |
| warm_latency_s | 1.42 | 1.41 | 1.42 | +0.01 |
| warm_model_s | 1.21 | 1.22 | 1.21 | -0.00 |
| warm_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| warm_harness_logic_s | 0.21 | 0.20 | 0.21 | +0.01 |
| warm_tool_s | 0.00 | 0.00 | 0.00 | +0.00 |
| agent_internal_s |  |  |  |  |
| task_s | 16.66 | 18.44 | 24.97 | +6.53 |
| harness_collect_s |  | 2.05 | 2.23 | +0.18 |
| isolation_s |  | 0.35 | 1.05 | +0.69 |
| evaluation_s | 0.21 | 0.16 | 0.87 | +0.71 |
| cleanup_s | 0.22 | 0.12 | 3.00 | +2.88 |
| cold_tool_mean_ms |  | 14.87 | 31.99 | +17.11 |
| warm_tool_mean_ms |  |  |  |  |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 1.70 | 1.15 | 0.71 | -0.44 |
| sandbox_cpu_peak | 6.15 | 17.00 | 6.42 | -10.58 |
| harness_cpu_peak | 157.77 | 152.02 | 174.03 | +22.01 |
| harness_memory_peak_mib | 250.10 | 258.65 | 259.50 | +0.85 |

### hermes, mb-one-call, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 16.47 | 16.03 | 18.41 | +2.39 |
| cold_startup_s | 8.04 | 8.03 | 10.62 | +2.58 |
| provisioning_s | 0.00 | 0.26 | 2.20 | +1.94 |
| harness_prepare_s |  | 0.02 | 1.52 | +1.51 |
| harness_start_s | 8.04 | 8.01 | 8.88 | +0.87 |
| cold_run_s | 8.42 | 7.99 | 7.80 | -0.20 |
| cold_model_s | 2.45 | 2.45 | 2.45 | -0.00 |
| cold_tool_exec_s | 0.51 | 0.42 | 0.70 | +0.28 |
| cold_harness_logic_s | 5.47 | 5.13 | 4.65 | -0.48 |
| cold_tool_s | 0.00 | 0.41 | 0.89 | +0.48 |
| warm_latency_s | 2.83 | 3.03 | 3.35 | +0.32 |
| warm_model_s | 2.45 | 2.45 | 2.45 | +0.00 |
| warm_tool_exec_s | 0.11 | 0.34 | 0.65 | +0.31 |
| warm_harness_logic_s | 0.27 | 0.24 | 0.25 | +0.01 |
| warm_tool_s | 0.00 | 0.21 | 0.45 | +0.24 |
| agent_internal_s |  |  |  |  |
| task_s | 19.93 | 21.72 | 28.99 | +7.26 |
| harness_collect_s |  | 2.04 | 2.24 | +0.20 |
| isolation_s |  | 0.36 | 1.05 | +0.70 |
| evaluation_s | 0.22 | 0.16 | 0.87 | +0.71 |
| cleanup_s | 0.26 | 0.11 | 3.06 | +2.95 |
| cold_tool_mean_ms |  | 14.71 | 31.79 | +17.07 |
| warm_tool_mean_ms |  | 14.73 | 31.83 | +17.10 |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 1.00 | 2.96 | 1.00 | -1.96 |
| sandbox_cpu_peak | 6.45 | 21.01 | 6.64 | -14.37 |
| harness_cpu_peak | 156.65 | 159.04 | 151.86 | -7.18 |
| harness_memory_peak_mib | 249.91 | 262.94 | 270.39 | +7.45 |

### hermes, mb-output, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 17.16 | 16.68 | 19.14 | +2.46 |
| cold_startup_s | 8.14 | 8.04 | 10.30 | +2.26 |
| provisioning_s | 0.00 | 0.26 | 2.18 | +1.92 |
| harness_prepare_s |  | 0.02 | 1.51 | +1.50 |
| harness_start_s | 8.14 | 8.02 | 8.57 | +0.55 |
| cold_run_s | 9.02 | 8.63 | 8.84 | +0.20 |
| cold_model_s | 3.10 | 3.10 | 3.10 | -0.00 |
| cold_tool_exec_s | 0.51 | 0.43 | 0.73 | +0.30 |
| cold_harness_logic_s | 5.40 | 5.11 | 5.01 | -0.10 |
| cold_tool_s | 0.00 | 0.41 | 0.90 | +0.49 |
| warm_latency_s | 3.45 | 3.64 | 3.99 | +0.35 |
| warm_model_s | 3.10 | 3.10 | 3.10 | -0.00 |
| warm_tool_exec_s | 0.12 | 0.35 | 0.66 | +0.31 |
| warm_harness_logic_s | 0.23 | 0.19 | 0.23 | +0.04 |
| warm_tool_s | 0.00 | 0.21 | 0.45 | +0.24 |
| agent_internal_s |  |  |  |  |
| task_s | 21.26 | 23.00 | 30.78 | +7.78 |
| harness_collect_s |  | 2.04 | 2.24 | +0.20 |
| isolation_s |  | 0.37 | 1.49 | +1.12 |
| evaluation_s | 0.23 | 0.16 | 0.87 | +0.71 |
| cleanup_s | 0.26 | 0.12 | 3.06 | +2.94 |
| cold_tool_mean_ms |  | 14.77 | 32.11 | +17.34 |
| warm_tool_mean_ms |  | 14.77 | 32.07 | +17.30 |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 0.46 | 2.62 | 0.96 | -1.66 |
| sandbox_cpu_peak | 4.52 | 21.72 | 6.47 | -15.24 |
| harness_cpu_peak | 162.05 | 165.04 | 154.57 | -10.47 |
| harness_memory_peak_mib | 247.58 | 258.65 | 266.23 | +7.59 |

### hermes, mb-ten-calls, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 28.23 | 27.80 | 30.97 | +3.17 |
| cold_startup_s | 8.10 | 8.04 | 11.03 | +3.00 |
| provisioning_s | 0.00 | 0.26 | 2.18 | +1.92 |
| harness_prepare_s |  | 0.02 | 1.51 | +1.50 |
| harness_start_s | 8.10 | 8.02 | 9.31 | +1.29 |
| cold_run_s | 20.13 | 19.76 | 19.94 | +0.17 |
| cold_model_s | 12.96 | 12.96 | 12.96 | +0.00 |
| cold_tool_exec_s | 1.51 | 1.46 | 2.57 | +1.11 |
| cold_harness_logic_s | 5.67 | 5.35 | 4.41 | -0.94 |
| cold_tool_s | 0.00 | 0.90 | 1.91 | +1.01 |
| warm_latency_s | 14.55 | 14.77 | 15.95 | +1.18 |
| warm_model_s | 12.96 | 12.96 | 12.96 | +0.00 |
| warm_tool_exec_s | 1.11 | 1.40 | 2.55 | +1.16 |
| warm_harness_logic_s | 0.48 | 0.42 | 0.44 | +0.02 |
| warm_tool_s | 0.00 | 0.69 | 1.47 | +0.77 |
| agent_internal_s |  |  |  |  |
| task_s | 43.44 | 45.27 | 54.22 | +8.96 |
| harness_collect_s |  | 2.04 | 2.23 | +0.19 |
| isolation_s |  | 0.37 | 1.15 | +0.77 |
| evaluation_s | 0.23 | 0.16 | 0.86 | +0.70 |
| cleanup_s | 0.27 | 0.12 | 3.07 | +2.95 |
| cold_tool_mean_ms |  | 14.08 | 29.90 | +15.82 |
| warm_tool_mean_ms |  | 13.89 | 29.36 | +15.47 |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 1.72 | 2.98 | 1.05 | -1.93 |
| sandbox_cpu_peak | 9.63 | 20.21 | 7.34 | -12.87 |
| harness_cpu_peak | 162.25 | 156.77 | 199.75 | +42.98 |
| harness_memory_peak_mib | 255.79 | 267.97 | 268.04 | +0.07 |

### openclaw, mb-cpu, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 36.97 | 39.36 | 40.84 | +1.48 |
| cold_startup_s | 27.21 | 30.21 | 31.68 | +1.47 |
| provisioning_s | 0.00 | 0.33 | 2.26 | +1.93 |
| harness_prepare_s |  | 0.03 | 2.50 | +2.47 |
| harness_start_s | 27.21 | 30.16 | 28.95 | -1.21 |
| cold_run_s | 9.76 | 9.15 | 9.16 | +0.01 |
| cold_model_s | 2.82 | 2.82 | 2.82 | +0.00 |
| cold_tool_exec_s | 3.74 | 3.88 | 4.04 | +0.16 |
| cold_harness_logic_s | 3.19 | 2.46 | 2.30 | -0.16 |
| cold_tool_s | 0.00 | 3.58 | 3.69 | +0.11 |
| warm_latency_s | 7.74 | 7.62 | 7.96 | +0.34 |
| warm_model_s | 2.82 | 2.82 | 2.82 | -0.00 |
| warm_tool_exec_s | 3.35 | 3.52 | 3.71 | +0.19 |
| warm_harness_logic_s | 1.57 | 1.28 | 1.43 | +0.15 |
| warm_tool_s | 0.00 | 3.39 | 3.58 | +0.19 |
| agent_internal_s | 7.66 |  |  |  |
| task_s | 45.10 | 48.11 | 55.03 | +6.92 |
| harness_collect_s |  | 0.01 | 0.30 | +0.29 |
| isolation_s |  | 0.86 | 2.00 | +1.15 |
| evaluation_s | 0.16 | 0.16 | 0.86 | +0.70 |
| cleanup_s | 0.19 | 0.11 | 3.07 | +2.96 |
| cold_tool_mean_ms |  | 715.88 | 737.48 | +21.60 |
| warm_tool_mean_ms |  | 677.44 | 715.76 | +38.32 |
| cold_harness_tool_mean_ms | 3523.60 | 3558.60 | 3563.00 | +4.40 |
| warm_harness_tool_mean_ms | 3286.80 | 3336.80 | 3411.00 | +74.20 |
| sandbox_cpu_mean | 44.73 | 14.44 | 12.90 | -1.54 |
| sandbox_cpu_peak | 100.10 | 100.08 | 100.08 | -0.00 |
| harness_cpu_peak | 175.28 | 206.28 | 173.10 | -33.18 |
| harness_memory_peak_mib | 773.46 | 902.20 | 995.23 | +93.02 |

### openclaw, mb-noop, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 32.20 | 34.36 | 35.61 | +1.25 |
| cold_startup_s | 27.35 | 30.12 | 31.58 | +1.45 |
| provisioning_s | 0.00 | 0.33 | 2.27 | +1.94 |
| harness_prepare_s |  | 0.03 | 2.49 | +2.47 |
| harness_start_s | 27.35 | 30.08 | 28.85 | -1.23 |
| cold_run_s | 4.86 | 4.23 | 4.04 | -0.20 |
| cold_model_s | 1.81 | 1.81 | 1.81 | +0.00 |
| cold_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| cold_harness_logic_s | 3.05 | 2.43 | 2.23 | -0.20 |
| cold_tool_s | 0.00 | 0.00 | 0.00 | +0.00 |
| warm_latency_s | 3.25 | 2.99 | 3.15 | +0.16 |
| warm_model_s | 1.81 | 1.81 | 1.81 | +0.00 |
| warm_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| warm_harness_logic_s | 1.44 | 1.18 | 1.34 | +0.16 |
| warm_tool_s | 0.00 | 0.00 | 0.00 | +0.00 |
| agent_internal_s | 3.09 |  |  |  |
| task_s | 35.87 | 38.32 | 44.60 | +6.28 |
| harness_collect_s |  | 0.01 | 0.30 | +0.29 |
| isolation_s |  | 0.70 | 1.74 | +1.04 |
| evaluation_s | 0.16 | 0.16 | 0.86 | +0.70 |
| cleanup_s | 0.22 | 0.11 | 2.94 | +2.83 |
| cold_tool_mean_ms |  |  |  |  |
| warm_tool_mean_ms |  |  |  |  |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 0.00 | 0.21 | 0.27 | +0.06 |
| sandbox_cpu_peak | 0.00 | 7.83 | 6.50 | -1.33 |
| harness_cpu_peak | 175.43 | 205.36 | 197.22 | -8.14 |
| harness_memory_peak_mib | 727.65 | 787.40 | 740.93 | -46.46 |

### openclaw, mb-one-call, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 33.15 | 35.41 | 37.99 | +2.58 |
| cold_startup_s | 27.28 | 30.13 | 32.65 | +2.51 |
| provisioning_s | 0.00 | 0.33 | 2.26 | +1.93 |
| harness_prepare_s |  | 0.03 | 2.50 | +2.47 |
| harness_start_s | 27.28 | 30.09 | 29.92 | -0.17 |
| cold_run_s | 5.86 | 5.27 | 5.34 | +0.06 |
| cold_model_s | 2.45 | 2.45 | 2.45 | -0.00 |
| cold_tool_exec_s | 0.26 | 0.41 | 0.59 | +0.18 |
| cold_harness_logic_s | 3.16 | 2.41 | 2.30 | -0.11 |
| cold_tool_s | 0.00 | 0.14 | 0.32 | +0.18 |
| warm_latency_s | 4.02 | 3.98 | 4.17 | +0.19 |
| warm_model_s | 2.45 | 2.45 | 2.45 | -0.00 |
| warm_tool_exec_s | 0.22 | 0.31 | 0.52 | +0.20 |
| warm_harness_logic_s | 1.35 | 1.22 | 1.21 | -0.02 |
| warm_tool_s | 0.00 | 0.15 | 0.33 | +0.18 |
| agent_internal_s | 3.95 |  |  |  |
| task_s | 37.58 | 40.46 | 48.21 | +7.75 |
| harness_collect_s |  | 0.01 | 0.30 | +0.29 |
| isolation_s |  | 0.78 | 1.83 | +1.05 |
| evaluation_s | 0.16 | 0.16 | 0.86 | +0.70 |
| cleanup_s | 0.21 | 0.11 | 3.07 | +2.96 |
| cold_tool_mean_ms |  | 28.12 | 64.60 | +36.48 |
| warm_tool_mean_ms |  | 29.12 | 66.00 | +36.88 |
| cold_harness_tool_mean_ms | 80.40 | 97.60 | 157.20 | +59.60 |
| warm_harness_tool_mean_ms | 101.20 | 115.60 | 195.60 | +80.00 |
| sandbox_cpu_mean | 1.23 | 0.75 | 0.39 | -0.36 |
| sandbox_cpu_peak | 4.90 | 11.52 | 6.55 | -4.97 |
| harness_cpu_peak | 173.96 | 205.91 | 168.99 | -36.92 |
| harness_memory_peak_mib | 815.80 | 1115.19 | 898.24 | -216.95 |

### openclaw, mb-output, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 32.98 | 35.49 | 36.89 | +1.40 |
| cold_startup_s | 27.17 | 30.22 | 31.58 | +1.36 |
| provisioning_s | 0.00 | 0.33 | 2.26 | +1.93 |
| harness_prepare_s |  | 0.03 | 2.50 | +2.47 |
| harness_start_s | 27.17 | 30.17 | 28.85 | -1.32 |
| cold_run_s | 5.80 | 5.27 | 5.31 | +0.04 |
| cold_model_s | 2.28 | 2.28 | 2.28 | -0.00 |
| cold_tool_exec_s | 0.36 | 0.54 | 0.81 | +0.27 |
| cold_harness_logic_s | 3.17 | 2.46 | 2.22 | -0.23 |
| cold_tool_s | 0.00 | 0.14 | 0.33 | +0.18 |
| warm_latency_s | 3.88 | 3.82 | 3.98 | +0.17 |
| warm_model_s | 2.28 | 2.28 | 2.28 | -0.00 |
| warm_tool_exec_s | 0.20 | 0.34 | 0.52 | +0.17 |
| warm_harness_logic_s | 1.40 | 1.20 | 1.19 | -0.01 |
| warm_tool_s | 0.00 | 0.15 | 0.32 | +0.18 |
| agent_internal_s | 3.80 |  |  |  |
| task_s | 37.30 | 40.29 | 47.01 | +6.71 |
| harness_collect_s |  | 0.01 | 0.30 | +0.29 |
| isolation_s |  | 0.70 | 1.91 | +1.21 |
| evaluation_s | 0.16 | 0.16 | 0.86 | +0.70 |
| cleanup_s | 0.24 | 0.11 | 3.06 | +2.95 |
| cold_tool_mean_ms |  | 28.92 | 65.60 | +36.68 |
| warm_tool_mean_ms |  | 29.40 | 64.48 | +35.08 |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 1.58 | 0.69 | 0.39 | -0.30 |
| sandbox_cpu_peak | 5.49 | 13.34 | 6.06 | -7.28 |
| harness_cpu_peak | 172.86 | 205.91 | 212.09 | +6.18 |
| harness_memory_peak_mib | 883.72 | 818.56 | 797.09 | -21.48 |

### openclaw, mb-ten-calls, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 46.60 | 49.92 | 52.48 | +2.56 |
| cold_startup_s | 27.14 | 30.32 | 31.64 | +1.32 |
| provisioning_s | 0.00 | 0.33 | 2.26 | +1.93 |
| harness_prepare_s |  | 0.03 | 2.50 | +2.47 |
| harness_start_s | 27.14 | 30.27 | 28.91 | -1.36 |
| cold_run_s | 19.45 | 19.60 | 20.84 | +1.24 |
| cold_model_s | 14.90 | 14.90 | 14.90 | -0.00 |
| cold_tool_exec_s | 1.25 | 2.07 | 3.50 | +1.42 |
| cold_harness_logic_s | 3.31 | 2.62 | 2.44 | -0.18 |
| cold_tool_s | 0.00 | 1.01 | 2.31 | +1.29 |
| warm_latency_s | 17.48 | 18.32 | 19.60 | +1.28 |
| warm_model_s | 14.90 | 14.90 | 14.90 | +0.00 |
| warm_tool_exec_s | 1.06 | 1.83 | 3.14 | +1.31 |
| warm_harness_logic_s | 1.52 | 1.59 | 1.56 | -0.03 |
| warm_tool_s | 0.00 | 1.02 | 2.29 | +1.27 |
| agent_internal_s | 17.41 |  |  |  |
| task_s | 64.47 | 69.32 | 78.06 | +8.74 |
| harness_collect_s |  | 0.01 | 0.30 | +0.29 |
| isolation_s |  | 0.79 | 1.77 | +0.97 |
| evaluation_s | 0.16 | 0.16 | 0.86 | +0.70 |
| cleanup_s | 0.19 | 0.11 | 3.06 | +2.95 |
| cold_tool_mean_ms |  | 24.74 | 56.25 | +31.51 |
| warm_tool_mean_ms |  | 24.86 | 55.79 | +30.93 |
| cold_harness_tool_mean_ms | 86.68 | 95.32 | 163.54 | +68.22 |
| warm_harness_tool_mean_ms | 81.84 | 88.50 | 150.86 | +62.36 |
| sandbox_cpu_mean | 2.51 | 2.43 | 0.66 | -1.77 |
| sandbox_cpu_peak | 6.13 | 13.15 | 6.82 | -6.34 |
| harness_cpu_peak | 176.05 | 206.95 | 172.68 | -34.28 |
| harness_memory_peak_mib | 767.92 | 963.35 | 815.33 | -148.01 |

Per-run rows: tasks.csv
