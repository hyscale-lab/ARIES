Means over replayed runs; times in seconds, CPU in percent of one core.

### hermes, mb-cpu, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 31.57 |  |
| cold_startup_s |  |  | 14.31 |  |
| provisioning_s |  |  | 3.13 |  |
| harness_prepare_s |  |  | 1.84 |  |
| harness_start_s |  |  | 12.03 |  |
| cold_run_s |  |  | 17.27 |  |
| cold_model_s |  |  | 2.66 |  |
| cold_tool_exec_s |  |  | 5.11 |  |
| cold_harness_logic_s |  |  | 9.50 |  |
| cold_tool_s |  |  | 5.46 |  |
| warm_latency_s |  |  | 8.02 |  |
| warm_model_s |  |  | 2.72 |  |
| warm_tool_exec_s |  |  | 4.92 |  |
| warm_harness_logic_s |  |  | 0.39 |  |
| warm_tool_s |  |  | 4.54 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 49.75 |  |
| harness_collect_s |  |  | 3.07 |  |
| isolation_s |  |  | 1.95 |  |
| evaluation_s |  |  | 1.68 |  |
| cleanup_s |  |  | 3.46 |  |
| cold_tool_mean_ms |  |  | 195.11 |  |
| warm_tool_mean_ms |  |  | 323.97 |  |
| cold_harness_tool_mean_ms |  |  | 5106.01 |  |
| warm_harness_tool_mean_ms |  |  | 4917.89 |  |
| sandbox_cpu_mean |  |  | 17.34 |  |
| sandbox_cpu_peak |  |  | 103.00 |  |
| harness_cpu_peak |  |  | 168.21 |  |
| harness_memory_peak_mib |  |  | 264.26 |  |

### hermes, mb-noop, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 25.22 |  |
| cold_startup_s |  |  | 14.49 |  |
| provisioning_s |  |  | 3.33 |  |
| harness_prepare_s |  |  | 2.02 |  |
| harness_start_s |  |  | 12.03 |  |
| cold_run_s |  |  | 10.73 |  |
| cold_model_s |  |  | 0.92 |  |
| cold_tool_exec_s |  |  | 0.00 |  |
| cold_harness_logic_s |  |  | 9.80 |  |
| cold_tool_s |  |  | 0.86 |  |
| warm_latency_s |  |  | 1.64 |  |
| warm_model_s |  |  | 1.28 |  |
| warm_tool_exec_s |  |  | 0.00 |  |
| warm_harness_logic_s |  |  | 0.36 |  |
| warm_tool_s |  |  | 0.00 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 36.91 |  |
| harness_collect_s |  |  | 3.05 |  |
| isolation_s |  |  | 1.99 |  |
| evaluation_s |  |  | 1.67 |  |
| cleanup_s |  |  | 3.33 |  |
| cold_tool_mean_ms |  |  | 61.53 |  |
| warm_tool_mean_ms |  |  |  |  |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean |  |  | 1.13 |  |
| sandbox_cpu_peak |  |  | 11.17 |  |
| harness_cpu_peak |  |  | 177.19 |  |
| harness_memory_peak_mib |  |  | 273.41 |  |

### hermes, mb-one-call, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 27.81 |  |
| cold_startup_s |  |  | 14.57 |  |
| provisioning_s |  |  | 3.19 |  |
| harness_prepare_s |  |  | 1.90 |  |
| harness_start_s |  |  | 12.24 |  |
| cold_run_s |  |  | 13.25 |  |
| cold_model_s |  |  | 2.40 |  |
| cold_tool_exec_s |  |  | 1.23 |  |
| cold_harness_logic_s |  |  | 9.61 |  |
| cold_tool_s |  |  | 1.73 |  |
| warm_latency_s |  |  | 3.63 |  |
| warm_model_s |  |  | 2.07 |  |
| warm_tool_exec_s |  |  | 1.17 |  |
| warm_harness_logic_s |  |  | 0.40 |  |
| warm_tool_s |  |  | 0.86 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 41.51 |  |
| harness_collect_s |  |  | 3.13 |  |
| isolation_s |  |  | 1.61 |  |
| evaluation_s |  |  | 1.67 |  |
| cleanup_s |  |  | 3.65 |  |
| cold_tool_mean_ms |  |  | 61.81 |  |
| warm_tool_mean_ms |  |  | 61.11 |  |
| cold_harness_tool_mean_ms |  |  | 1232.13 |  |
| warm_harness_tool_mean_ms |  |  | 1166.98 |  |
| sandbox_cpu_mean |  |  | 1.46 |  |
| sandbox_cpu_peak |  |  | 9.91 |  |
| harness_cpu_peak |  |  | 176.89 |  |
| harness_memory_peak_mib |  |  | 267.91 |  |

### hermes, mb-output, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 27.38 |  |
| cold_startup_s |  |  | 14.32 |  |
| provisioning_s |  |  | 3.19 |  |
| harness_prepare_s |  |  | 1.90 |  |
| harness_start_s |  |  | 11.99 |  |
| cold_run_s |  |  | 13.06 |  |
| cold_model_s |  |  | 2.33 |  |
| cold_tool_exec_s |  |  | 1.24 |  |
| cold_harness_logic_s |  |  | 9.49 |  |
| cold_tool_s |  |  | 1.73 |  |
| warm_latency_s |  |  | 3.93 |  |
| warm_model_s |  |  | 2.39 |  |
| warm_tool_exec_s |  |  | 1.16 |  |
| warm_harness_logic_s |  |  | 0.37 |  |
| warm_tool_s |  |  | 0.86 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 41.51 |  |
| harness_collect_s |  |  | 3.11 |  |
| isolation_s |  |  | 1.83 |  |
| evaluation_s |  |  | 1.68 |  |
| cleanup_s |  |  | 3.58 |  |
| cold_tool_mean_ms |  |  | 61.84 |  |
| warm_tool_mean_ms |  |  | 61.64 |  |
| cold_harness_tool_mean_ms |  |  | 1241.47 |  |
| warm_harness_tool_mean_ms |  |  | 1161.74 |  |
| sandbox_cpu_mean |  |  | 1.66 |  |
| sandbox_cpu_peak |  |  | 10.69 |  |
| harness_cpu_peak |  |  | 198.94 |  |
| harness_memory_peak_mib |  |  | 269.27 |  |

### hermes, mb-ten-calls, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 42.65 |  |
| cold_startup_s |  |  | 14.48 |  |
| provisioning_s |  |  | 3.07 |  |
| harness_prepare_s |  |  | 1.78 |  |
| harness_start_s |  |  | 12.26 |  |
| cold_run_s |  |  | 28.17 |  |
| cold_model_s |  |  | 13.93 |  |
| cold_tool_exec_s |  |  | 4.35 |  |
| cold_harness_logic_s |  |  | 9.89 |  |
| cold_tool_s |  |  | 3.71 |  |
| warm_latency_s |  |  | 18.84 |  |
| warm_model_s |  |  | 13.72 |  |
| warm_tool_exec_s |  |  | 4.32 |  |
| warm_harness_logic_s |  |  | 0.80 |  |
| warm_tool_s |  |  | 2.83 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 71.43 |  |
| harness_collect_s |  |  | 3.06 |  |
| isolation_s |  |  | 1.83 |  |
| evaluation_s |  |  | 1.67 |  |
| cleanup_s |  |  | 3.38 |  |
| cold_tool_mean_ms |  |  | 57.93 |  |
| warm_tool_mean_ms |  |  | 56.62 |  |
| cold_harness_tool_mean_ms |  |  | 435.39 |  |
| warm_harness_tool_mean_ms |  |  | 431.93 |  |
| sandbox_cpu_mean |  |  | 1.70 |  |
| sandbox_cpu_peak |  |  | 10.99 |  |
| harness_cpu_peak |  |  | 190.14 |  |
| harness_memory_peak_mib |  |  | 270.44 |  |

### openclaw, mb-cpu, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 73.53 |  |
| cold_startup_s |  |  | 59.82 |  |
| provisioning_s |  |  | 3.43 |  |
| harness_prepare_s |  |  | 2.93 |  |
| harness_start_s |  |  | 56.41 |  |
| cold_run_s |  |  | 13.71 |  |
| cold_model_s |  |  | 4.90 |  |
| cold_tool_exec_s |  |  | 5.17 |  |
| cold_harness_logic_s |  |  | 3.64 |  |
| cold_tool_s |  |  | 4.41 |  |
| warm_latency_s |  |  | 10.55 |  |
| warm_model_s |  |  | 3.86 |  |
| warm_tool_exec_s |  |  | 4.57 |  |
| warm_harness_logic_s |  |  | 2.12 |  |
| warm_tool_s |  |  | 4.28 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 92.34 |  |
| harness_collect_s |  |  | 0.57 |  |
| isolation_s |  |  | 2.75 |  |
| evaluation_s |  |  | 1.65 |  |
| cleanup_s |  |  | 3.28 |  |
| cold_tool_mean_ms |  |  | 881.08 |  |
| warm_tool_mean_ms |  |  | 855.64 |  |
| cold_harness_tool_mean_ms |  |  | 4201.00 |  |
| warm_harness_tool_mean_ms |  |  | 3975.00 |  |
| sandbox_cpu_mean |  |  | 8.91 |  |
| sandbox_cpu_peak |  |  | 135.57 |  |
| harness_cpu_peak |  |  | 290.31 |  |
| harness_memory_peak_mib |  |  | 1028.23 |  |

### openclaw, mb-noop, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 64.30 |  |
| cold_startup_s |  |  | 59.84 |  |
| provisioning_s |  |  | 3.48 |  |
| harness_prepare_s |  |  | 2.98 |  |
| harness_start_s |  |  | 56.39 |  |
| cold_run_s |  |  | 4.46 |  |
| cold_model_s |  |  | 1.19 |  |
| cold_tool_exec_s |  |  | 0.00 |  |
| cold_harness_logic_s |  |  | 3.27 |  |
| cold_tool_s |  |  | 0.00 |  |
| warm_latency_s |  |  | 2.32 |  |
| warm_model_s |  |  | 0.74 |  |
| warm_tool_exec_s |  |  | 0.00 |  |
| warm_harness_logic_s |  |  | 1.58 |  |
| warm_tool_s |  |  | 0.00 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 74.76 |  |
| harness_collect_s |  |  | 0.57 |  |
| isolation_s |  |  | 2.63 |  |
| evaluation_s |  |  | 1.65 |  |
| cleanup_s |  |  | 3.29 |  |
| cold_tool_mean_ms |  |  |  |  |
| warm_tool_mean_ms |  |  |  |  |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean |  |  | 0.34 |  |
| sandbox_cpu_peak |  |  | 9.87 |  |
| harness_cpu_peak |  |  | 250.28 |  |
| harness_memory_peak_mib |  |  | 793.34 |  |

### openclaw, mb-one-call, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 66.91 |  |
| cold_startup_s |  |  | 60.16 |  |
| provisioning_s |  |  | 3.42 |  |
| harness_prepare_s |  |  | 2.93 |  |
| harness_start_s |  |  | 56.76 |  |
| cold_run_s |  |  | 6.75 |  |
| cold_model_s |  |  | 2.35 |  |
| cold_tool_exec_s |  |  | 1.20 |  |
| cold_harness_logic_s |  |  | 3.19 |  |
| cold_tool_s |  |  | 0.62 |  |
| warm_latency_s |  |  | 5.41 |  |
| warm_model_s |  |  | 2.45 |  |
| warm_tool_exec_s |  |  | 1.01 |  |
| warm_harness_logic_s |  |  | 1.95 |  |
| warm_tool_s |  |  | 0.61 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 80.71 |  |
| harness_collect_s |  |  | 0.56 |  |
| isolation_s |  |  | 2.76 |  |
| evaluation_s |  |  | 1.65 |  |
| cleanup_s |  |  | 3.42 |  |
| cold_tool_mean_ms |  |  | 123.40 |  |
| warm_tool_mean_ms |  |  | 122.92 |  |
| cold_harness_tool_mean_ms |  |  | 318.20 |  |
| warm_harness_tool_mean_ms |  |  | 395.00 |  |
| sandbox_cpu_mean |  |  | 0.45 |  |
| sandbox_cpu_peak |  |  | 9.31 |  |
| harness_cpu_peak |  |  | 225.77 |  |
| harness_memory_peak_mib |  |  | 931.03 |  |

### openclaw, mb-output, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 67.90 |  |
| cold_startup_s |  |  | 60.01 |  |
| provisioning_s |  |  | 3.41 |  |
| harness_prepare_s |  |  | 2.92 |  |
| harness_start_s |  |  | 56.61 |  |
| cold_run_s |  |  | 7.89 |  |
| cold_model_s |  |  | 2.96 |  |
| cold_tool_exec_s |  |  | 1.55 |  |
| cold_harness_logic_s |  |  | 3.37 |  |
| cold_tool_s |  |  | 0.61 |  |
| warm_latency_s |  |  | 6.20 |  |
| warm_model_s |  |  | 3.08 |  |
| warm_tool_exec_s |  |  | 1.05 |  |
| warm_harness_logic_s |  |  | 2.07 |  |
| warm_tool_s |  |  | 0.62 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 82.77 |  |
| harness_collect_s |  |  | 0.57 |  |
| isolation_s |  |  | 3.11 |  |
| evaluation_s |  |  | 1.65 |  |
| cleanup_s |  |  | 3.34 |  |
| cold_tool_mean_ms |  |  | 122.64 |  |
| warm_tool_mean_ms |  |  | 123.40 |  |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean |  |  | 0.47 |  |
| sandbox_cpu_peak |  |  | 10.65 |  |
| harness_cpu_peak |  |  | 302.55 |  |
| harness_memory_peak_mib |  |  | 942.64 |  |

### openclaw, mb-ten-calls, warm-parallel (solved: native 0/0, ARIES Docker 0/0, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s |  |  | 85.59 |  |
| cold_startup_s |  |  | 60.01 |  |
| provisioning_s |  |  | 3.42 |  |
| harness_prepare_s |  |  | 2.93 |  |
| harness_start_s |  |  | 56.60 |  |
| cold_run_s |  |  | 25.59 |  |
| cold_model_s |  |  | 14.90 |  |
| cold_tool_exec_s |  |  | 6.83 |  |
| cold_harness_logic_s |  |  | 3.86 |  |
| cold_tool_s |  |  | 4.34 |  |
| warm_latency_s |  |  | 24.58 |  |
| warm_model_s |  |  | 15.79 |  |
| warm_tool_exec_s |  |  | 6.08 |  |
| warm_harness_logic_s |  |  | 2.71 |  |
| warm_tool_s |  |  | 4.30 |  |
| agent_internal_s |  |  |  |  |
| task_s |  |  | 118.82 |  |
| harness_collect_s |  |  | 0.58 |  |
| isolation_s |  |  | 3.00 |  |
| evaluation_s |  |  | 1.66 |  |
| cleanup_s |  |  | 3.41 |  |
| cold_tool_mean_ms |  |  | 105.78 |  |
| warm_tool_mean_ms |  |  | 104.95 |  |
| cold_harness_tool_mean_ms |  |  | 323.20 |  |
| warm_harness_tool_mean_ms |  |  | 303.94 |  |
| sandbox_cpu_mean |  |  | 0.97 |  |
| sandbox_cpu_peak |  |  | 10.51 |  |
| harness_cpu_peak |  |  | 199.79 |  |
| harness_memory_peak_mib |  |  | 923.19 |  |

Per-run rows: tasks.csv
