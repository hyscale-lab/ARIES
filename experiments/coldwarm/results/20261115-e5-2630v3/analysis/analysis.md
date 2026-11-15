Means over replayed runs; times in seconds, CPU in percent of one core.

### hermes, mb-cpu, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 28.74 | 27.73 | 31.78 | +4.05 |
| cold_startup_s | 10.97 | 10.68 | 14.34 | +3.65 |
| provisioning_s | 0.00 | 0.52 | 3.15 | +2.62 |
| harness_prepare_s |  | 0.04 | 1.86 | +1.82 |
| harness_start_s | 10.97 | 10.64 | 12.05 | +1.41 |
| cold_run_s | 17.76 | 17.05 | 17.44 | +0.40 |
| cold_model_s | 2.66 | 2.66 | 2.66 | -0.00 |
| cold_tool_exec_s | 4.77 | 4.55 | 5.13 | +0.58 |
| cold_harness_logic_s | 10.33 | 9.84 | 9.65 | -0.18 |
| cold_tool_s | 0.00 | 4.50 | 5.51 | +1.01 |
| warm_latency_s | 7.07 | 7.73 | 8.00 | +0.27 |
| warm_model_s | 2.72 | 2.72 | 2.72 | -0.00 |
| warm_tool_exec_s | 3.98 | 4.57 | 4.86 | +0.29 |
| warm_harness_logic_s | 0.37 | 0.44 | 0.43 | -0.01 |
| warm_tool_s | 0.00 | 4.16 | 4.46 | +0.31 |
| agent_internal_s |  |  |  |  |
| task_s | 37.08 | 39.23 | 49.71 | +10.47 |
| harness_collect_s |  | 2.70 | 3.06 | +0.37 |
| isolation_s |  | 0.57 | 1.78 | +1.21 |
| evaluation_s | 0.48 | 0.33 | 1.67 | +1.34 |
| cleanup_s | 0.46 | 0.18 | 3.42 | +3.23 |
| cold_tool_mean_ms |  | 160.74 | 196.76 | +36.03 |
| warm_tool_mean_ms |  | 296.90 | 318.90 | +22.00 |
| cold_harness_tool_mean_ms | 4771.26 | 4549.90 | 5133.25 | +583.36 |
| warm_harness_tool_mean_ms | 3983.38 | 4567.82 | 4855.78 | +287.96 |
| sandbox_cpu_mean | 26.49 | 23.41 | 17.88 | -5.53 |
| sandbox_cpu_peak | 101.45 | 113.71 | 100.11 | -13.60 |
| harness_cpu_peak | 178.67 | 183.49 | 199.68 | +16.20 |
| harness_memory_peak_mib | 242.53 | 259.24 | 273.12 | +13.88 |

### hermes, mb-noop, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 22.06 | 21.42 | 24.97 | +3.56 |
| cold_startup_s | 10.91 | 10.63 | 14.38 | +3.76 |
| provisioning_s | 0.00 | 0.51 | 3.34 | +2.83 |
| harness_prepare_s |  | 0.03 | 2.04 | +2.01 |
| harness_start_s | 10.91 | 10.59 | 11.91 | +1.32 |
| cold_run_s | 11.15 | 10.79 | 10.59 | -0.20 |
| cold_model_s | 0.92 | 0.92 | 0.92 | +0.00 |
| cold_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| cold_harness_logic_s | 10.23 | 9.87 | 9.67 | -0.20 |
| cold_tool_s | 0.00 | 0.44 | 0.86 | +0.42 |
| warm_latency_s | 1.64 | 1.62 | 1.64 | +0.02 |
| warm_model_s | 1.28 | 1.28 | 1.28 | -0.00 |
| warm_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| warm_harness_logic_s | 0.36 | 0.34 | 0.36 | +0.02 |
| warm_tool_s | 0.00 | 0.00 | 0.00 | +0.00 |
| agent_internal_s |  |  |  |  |
| task_s | 24.88 | 26.83 | 36.77 | +9.95 |
| harness_collect_s |  | 2.71 | 3.06 | +0.35 |
| isolation_s |  | 0.55 | 2.04 | +1.48 |
| evaluation_s | 0.43 | 0.33 | 1.66 | +1.33 |
| cleanup_s | 0.40 | 0.19 | 3.40 | +3.21 |
| cold_tool_mean_ms |  | 31.16 | 61.29 | +30.13 |
| warm_tool_mean_ms |  |  |  |  |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 2.57 | 1.36 | 1.21 | -0.15 |
| sandbox_cpu_peak | 10.51 | 23.38 | 10.42 | -12.96 |
| harness_cpu_peak | 171.55 | 165.47 | 210.81 | +45.34 |
| harness_memory_peak_mib | 251.31 | 264.40 | 273.09 | +8.69 |

### hermes, mb-one-call, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 24.65 | 23.85 | 27.77 | +3.92 |
| cold_startup_s | 10.93 | 10.86 | 14.43 | +3.57 |
| provisioning_s | 0.00 | 0.51 | 3.14 | +2.63 |
| harness_prepare_s |  | 0.04 | 1.86 | +1.82 |
| harness_start_s | 10.93 | 10.81 | 12.14 | +1.33 |
| cold_run_s | 13.72 | 12.99 | 13.34 | +0.35 |
| cold_model_s | 2.40 | 2.40 | 2.40 | -0.00 |
| cold_tool_exec_s | 0.95 | 0.86 | 1.20 | +0.34 |
| cold_harness_logic_s | 10.37 | 9.73 | 9.74 | +0.00 |
| cold_tool_s | 0.00 | 0.86 | 1.70 | +0.84 |
| warm_latency_s | 2.67 | 3.25 | 3.61 | +0.35 |
| warm_model_s | 2.07 | 2.07 | 2.07 | +0.00 |
| warm_tool_exec_s | 0.17 | 0.77 | 1.14 | +0.37 |
| warm_harness_logic_s | 0.43 | 0.42 | 0.39 | -0.02 |
| warm_tool_s | 0.00 | 0.43 | 0.85 | +0.42 |
| agent_internal_s |  |  |  |  |
| task_s | 28.57 | 30.98 | 41.40 | +10.42 |
| harness_collect_s |  | 2.77 | 3.08 | +0.31 |
| isolation_s |  | 0.58 | 1.74 | +1.16 |
| evaluation_s | 0.48 | 0.33 | 1.66 | +1.33 |
| cleanup_s | 0.43 | 0.20 | 3.54 | +3.35 |
| cold_tool_mean_ms |  | 30.79 | 60.84 | +30.05 |
| warm_tool_mean_ms |  | 30.53 | 60.54 | +30.01 |
| cold_harness_tool_mean_ms | 953.30 | 857.14 | 1201.02 | +343.88 |
| warm_harness_tool_mean_ms | 173.78 | 767.14 | 1141.21 | +374.07 |
| sandbox_cpu_mean | 2.12 | 3.70 | 1.61 | -2.09 |
| sandbox_cpu_peak | 15.61 | 34.08 | 10.65 | -23.44 |
| harness_cpu_peak | 180.66 | 242.46 | 169.58 | -72.89 |
| harness_memory_peak_mib | 267.93 | 254.89 | 268.32 | +13.43 |

### hermes, mb-output, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 24.47 | 23.41 | 27.70 | +4.29 |
| cold_startup_s | 10.80 | 10.34 | 14.43 | +4.09 |
| provisioning_s | 0.00 | 0.52 | 3.28 | +2.76 |
| harness_prepare_s |  | 0.04 | 1.98 | +1.95 |
| harness_start_s | 10.80 | 10.29 | 12.01 | +1.72 |
| cold_run_s | 13.66 | 13.08 | 13.28 | +0.20 |
| cold_model_s | 2.33 | 2.33 | 2.33 | -0.00 |
| cold_tool_exec_s | 1.00 | 0.88 | 1.23 | +0.35 |
| cold_harness_logic_s | 10.34 | 9.87 | 9.72 | -0.15 |
| cold_tool_s | 0.00 | 0.87 | 1.72 | +0.85 |
| warm_latency_s | 2.98 | 3.56 | 3.93 | +0.37 |
| warm_model_s | 2.39 | 2.39 | 2.39 | +0.00 |
| warm_tool_exec_s | 0.19 | 0.78 | 1.15 | +0.38 |
| warm_harness_logic_s | 0.40 | 0.39 | 0.38 | -0.01 |
| warm_tool_s | 0.00 | 0.43 | 0.85 | +0.42 |
| agent_internal_s |  |  |  |  |
| task_s | 28.74 | 30.81 | 41.55 | +10.74 |
| harness_collect_s |  | 2.76 | 3.05 | +0.30 |
| isolation_s |  | 0.56 | 1.85 | +1.29 |
| evaluation_s | 0.48 | 0.33 | 1.66 | +1.33 |
| cleanup_s | 0.48 | 0.19 | 3.36 | +3.17 |
| cold_tool_mean_ms |  | 30.96 | 61.40 | +30.44 |
| warm_tool_mean_ms |  | 30.66 | 60.56 | +29.90 |
| cold_harness_tool_mean_ms | 1002.74 | 882.44 | 1232.75 | +350.31 |
| warm_harness_tool_mean_ms | 191.71 | 775.36 | 1154.87 | +379.51 |
| sandbox_cpu_mean | 2.52 | 4.39 | 1.71 | -2.68 |
| sandbox_cpu_peak | 17.91 | 40.04 | 11.07 | -28.97 |
| harness_cpu_peak | 176.99 | 177.55 | 188.47 | +10.92 |
| harness_memory_peak_mib | 264.56 | 266.37 | 270.98 | +4.61 |

### hermes, mb-ten-calls, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 37.91 | 38.06 | 42.62 | +4.56 |
| cold_startup_s | 10.82 | 10.74 | 14.76 | +4.02 |
| provisioning_s | 0.00 | 0.52 | 3.15 | +2.62 |
| harness_prepare_s |  | 0.03 | 1.85 | +1.82 |
| harness_start_s | 10.82 | 10.70 | 12.48 | +1.78 |
| cold_run_s | 27.09 | 27.32 | 27.86 | +0.54 |
| cold_model_s | 13.93 | 13.93 | 13.93 | +0.00 |
| cold_tool_exec_s | 2.51 | 3.23 | 4.34 | +1.11 |
| cold_harness_logic_s | 10.64 | 10.16 | 9.59 | -0.57 |
| cold_tool_s | 0.00 | 1.84 | 3.66 | +1.82 |
| warm_latency_s | 16.24 | 17.65 | 18.83 | +1.18 |
| warm_model_s | 13.72 | 13.72 | 13.72 | -0.00 |
| warm_tool_exec_s | 1.74 | 3.14 | 4.33 | +1.18 |
| warm_harness_logic_s | 0.78 | 0.79 | 0.79 | -0.00 |
| warm_tool_s | 0.00 | 1.42 | 2.80 | +1.39 |
| agent_internal_s |  |  |  |  |
| task_s | 55.41 | 59.52 | 71.29 | +11.77 |
| harness_collect_s |  | 2.72 | 3.08 | +0.36 |
| isolation_s |  | 0.57 | 1.70 | +1.14 |
| evaluation_s | 0.48 | 0.33 | 1.67 | +1.33 |
| cleanup_s | 0.45 | 0.18 | 3.38 | +3.20 |
| cold_tool_mean_ms |  | 28.72 | 57.20 | +28.48 |
| warm_tool_mean_ms |  | 28.31 | 56.01 | +27.70 |
| cold_harness_tool_mean_ms | 251.33 | 322.63 | 433.80 | +111.18 |
| warm_harness_tool_mean_ms | 173.73 | 314.24 | 432.55 | +118.30 |
| sandbox_cpu_mean | 3.17 | 4.55 | 1.72 | -2.83 |
| sandbox_cpu_peak | 20.00 | 35.41 | 12.10 | -23.31 |
| harness_cpu_peak | 177.57 | 173.47 | 176.41 | +2.94 |
| harness_memory_peak_mib | 270.05 | 266.71 | 271.74 | +5.03 |

### openclaw, mb-cpu, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 66.19 | 69.38 | 69.79 | +0.41 |
| cold_startup_s | 51.61 | 56.44 | 56.27 | -0.17 |
| provisioning_s | 0.00 | 0.63 | 3.25 | +2.62 |
| harness_prepare_s |  | 0.04 | 2.76 | +2.72 |
| harness_start_s | 51.61 | 56.34 | 53.04 | -3.30 |
| cold_run_s | 14.58 | 12.94 | 13.51 | +0.57 |
| cold_model_s | 4.90 | 4.90 | 4.90 | +0.00 |
| cold_tool_exec_s | 4.33 | 4.62 | 5.01 | +0.39 |
| cold_harness_logic_s | 5.35 | 3.42 | 3.60 | +0.18 |
| cold_tool_s | 0.00 | 3.94 | 4.32 | +0.39 |
| warm_latency_s | 10.61 | 10.22 | 10.59 | +0.37 |
| warm_model_s | 3.86 | 3.86 | 3.86 | +0.00 |
| warm_tool_exec_s | 3.98 | 4.36 | 4.62 | +0.26 |
| warm_harness_logic_s | 2.78 | 2.00 | 2.12 | +0.11 |
| warm_tool_s | 0.00 | 4.01 | 4.31 | +0.30 |
| agent_internal_s | 10.40 |  |  |  |
| task_s | 77.60 | 81.78 | 88.97 | +7.19 |
| harness_collect_s |  | 0.02 | 0.58 | +0.56 |
| isolation_s |  | 1.65 | 3.07 | +1.43 |
| evaluation_s | 0.35 | 0.33 | 1.65 | +1.32 |
| cleanup_s | 0.36 | 0.18 | 3.28 | +3.10 |
| cold_tool_mean_ms |  | 787.36 | 864.68 | +77.32 |
| warm_tool_mean_ms |  | 801.40 | 861.80 | +60.40 |
| cold_harness_tool_mean_ms | 3759.00 | 3894.80 | 4105.20 | +210.40 |
| warm_harness_tool_mean_ms | 3844.00 | 3962.20 | 4013.40 | +51.20 |
| sandbox_cpu_mean | 35.02 | 9.25 | 9.17 | -0.09 |
| sandbox_cpu_peak | 100.02 | 100.01 | 100.05 | +0.04 |
| harness_cpu_peak | 199.86 | 208.11 | 191.58 | -16.53 |
| harness_memory_peak_mib | 990.80 | 999.14 | 857.51 | -141.63 |

### openclaw, mb-noop, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 57.48 | 60.24 | 61.50 | +1.27 |
| cold_startup_s | 51.31 | 55.47 | 56.90 | +1.43 |
| provisioning_s | 0.00 | 0.63 | 3.52 | +2.89 |
| harness_prepare_s |  | 0.04 | 3.00 | +2.96 |
| harness_start_s | 51.31 | 55.40 | 53.42 | -1.98 |
| cold_run_s | 6.17 | 4.77 | 4.61 | -0.16 |
| cold_model_s | 1.19 | 1.19 | 1.19 | -0.00 |
| cold_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| cold_harness_logic_s | 4.98 | 3.58 | 3.42 | -0.16 |
| cold_tool_s | 0.00 | 0.00 | 0.00 | +0.00 |
| warm_latency_s | 3.16 | 2.49 | 2.40 | -0.09 |
| warm_model_s | 0.74 | 0.74 | 0.73 | -0.00 |
| warm_tool_exec_s | 0.00 | 0.00 | 0.00 | +0.00 |
| warm_harness_logic_s | 2.43 | 1.75 | 1.67 | -0.09 |
| warm_tool_s | 0.00 | 0.00 | 0.00 | +0.00 |
| agent_internal_s | 3.01 |  |  |  |
| task_s | 61.47 | 64.55 | 72.29 | +7.74 |
| harness_collect_s |  | 0.02 | 0.57 | +0.55 |
| isolation_s |  | 1.29 | 2.66 | +1.37 |
| evaluation_s | 0.34 | 0.33 | 1.65 | +1.32 |
| cleanup_s | 0.40 | 0.18 | 3.51 | +3.33 |
| cold_tool_mean_ms |  |  |  |  |
| warm_tool_mean_ms |  |  |  |  |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 0.29 | 0.30 | 0.38 | +0.08 |
| sandbox_cpu_peak | 1.73 | 15.62 | 10.34 | -5.28 |
| harness_cpu_peak | 195.17 | 206.60 | 220.46 | +13.86 |
| harness_memory_peak_mib | 727.80 | 774.11 | 764.13 | -9.98 |

### openclaw, mb-one-call, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 59.20 | 63.02 | 63.96 | +0.94 |
| cold_startup_s | 51.47 | 56.60 | 57.23 | +0.63 |
| provisioning_s | 0.00 | 0.63 | 3.50 | +2.87 |
| harness_prepare_s |  | 0.04 | 3.01 | +2.96 |
| harness_start_s | 51.47 | 56.51 | 53.75 | -2.76 |
| cold_run_s | 7.72 | 6.42 | 6.73 | +0.31 |
| cold_model_s | 2.35 | 2.35 | 2.35 | -0.00 |
| cold_tool_exec_s | 0.55 | 0.87 | 1.20 | +0.33 |
| cold_harness_logic_s | 4.81 | 3.19 | 3.17 | -0.01 |
| cold_tool_s | 0.00 | 0.30 | 0.62 | +0.32 |
| warm_latency_s | 4.99 | 4.77 | 5.33 | +0.56 |
| warm_model_s | 2.45 | 2.45 | 2.45 | +0.00 |
| warm_tool_exec_s | 0.29 | 0.63 | 0.98 | +0.34 |
| warm_harness_logic_s | 2.24 | 1.68 | 1.90 | +0.22 |
| warm_tool_s | 0.00 | 0.30 | 0.62 | +0.32 |
| agent_internal_s | 4.83 |  |  |  |
| task_s | 65.01 | 69.85 | 77.97 | +8.12 |
| harness_collect_s |  | 0.02 | 0.56 | +0.54 |
| isolation_s |  | 1.53 | 2.85 | +1.32 |
| evaluation_s | 0.33 | 0.33 | 1.65 | +1.32 |
| cleanup_s | 0.40 | 0.18 | 3.62 | +3.44 |
| cold_tool_mean_ms |  | 60.32 | 124.36 | +64.04 |
| warm_tool_mean_ms |  | 60.08 | 124.12 | +64.04 |
| cold_harness_tool_mean_ms | 174.20 | 212.60 | 319.00 | +106.40 |
| warm_harness_tool_mean_ms | 164.60 | 241.00 | 370.40 | +129.40 |
| sandbox_cpu_mean | 2.19 | 0.80 | 0.54 | -0.26 |
| sandbox_cpu_peak | 9.75 | 24.83 | 10.63 | -14.20 |
| harness_cpu_peak | 196.72 | 209.02 | 189.18 | -19.84 |
| harness_memory_peak_mib | 989.99 | 911.96 | 1011.51 | +99.56 |

### openclaw, mb-output, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 59.91 | 64.50 | 64.67 | +0.17 |
| cold_startup_s | 51.21 | 56.89 | 57.31 | +0.43 |
| provisioning_s | 0.00 | 0.63 | 3.44 | +2.80 |
| harness_prepare_s |  | 0.04 | 2.94 | +2.90 |
| harness_start_s | 51.21 | 56.79 | 53.90 | -2.89 |
| cold_run_s | 8.70 | 7.61 | 7.36 | -0.26 |
| cold_model_s | 2.96 | 2.97 | 2.96 | -0.00 |
| cold_tool_exec_s | 0.79 | 1.15 | 1.47 | +0.33 |
| cold_harness_logic_s | 4.94 | 3.50 | 2.92 | -0.58 |
| cold_tool_s | 0.00 | 0.30 | 0.62 | +0.32 |
| warm_latency_s | 5.75 | 5.55 | 5.99 | +0.44 |
| warm_model_s | 3.08 | 3.08 | 3.08 | -0.00 |
| warm_tool_exec_s | 0.43 | 0.74 | 1.06 | +0.32 |
| warm_harness_logic_s | 2.24 | 1.73 | 1.85 | +0.12 |
| warm_tool_s | 0.00 | 0.30 | 0.62 | +0.33 |
| agent_internal_s | 5.60 |  |  |  |
| task_s | 66.51 | 72.09 | 79.11 | +7.02 |
| harness_collect_s |  | 0.02 | 0.57 | +0.55 |
| isolation_s |  | 1.50 | 2.95 | +1.45 |
| evaluation_s | 0.34 | 0.34 | 1.65 | +1.31 |
| cleanup_s | 0.42 | 0.18 | 3.28 | +3.10 |
| cold_tool_mean_ms |  | 59.68 | 123.72 | +64.04 |
| warm_tool_mean_ms |  | 59.52 | 124.56 | +65.04 |
| cold_harness_tool_mean_ms |  |  |  |  |
| warm_harness_tool_mean_ms |  |  |  |  |
| sandbox_cpu_mean | 1.96 | 0.88 | 0.56 | -0.31 |
| sandbox_cpu_peak | 10.91 | 26.71 | 11.22 | -15.49 |
| harness_cpu_peak | 193.55 | 208.13 | 253.13 | +45.00 |
| harness_memory_peak_mib | 1054.55 | 1077.32 | 973.83 | -103.49 |

### openclaw, mb-ten-calls, warm-parallel (solved: native 5/5, ARIES Docker 5/5, ARIES Kubernetes 5/5)

| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |
| --- | --- | --- | --- | --- |
| cold_latency_s | 73.83 | 79.35 | 82.43 | +3.08 |
| cold_startup_s | 50.82 | 56.21 | 57.26 | +1.05 |
| provisioning_s | 0.00 | 0.63 | 3.43 | +2.80 |
| harness_prepare_s |  | 0.04 | 2.94 | +2.89 |
| harness_start_s | 50.82 | 56.11 | 53.84 | -2.27 |
| cold_run_s | 23.01 | 23.14 | 25.17 | +2.03 |
| cold_model_s | 14.90 | 14.90 | 14.90 | -0.00 |
| cold_tool_exec_s | 2.68 | 4.43 | 6.65 | +2.22 |
| cold_harness_logic_s | 5.44 | 3.81 | 3.62 | -0.18 |
| cold_tool_s | 0.00 | 2.10 | 4.34 | +2.24 |
| warm_latency_s | 21.16 | 22.37 | 24.46 | +2.09 |
| warm_model_s | 15.79 | 15.79 | 15.79 | +0.00 |
| warm_tool_exec_s | 2.15 | 3.90 | 6.10 | +2.20 |
| warm_harness_logic_s | 3.23 | 2.69 | 2.57 | -0.12 |
| warm_tool_s | 0.00 | 2.09 | 4.34 | +2.25 |
| agent_internal_s | 21.00 |  |  |  |
| task_s | 95.77 | 103.87 | 115.39 | +11.52 |
| harness_collect_s |  | 0.02 | 0.59 | +0.56 |
| isolation_s |  | 1.61 | 2.81 | +1.20 |
| evaluation_s | 0.34 | 0.33 | 1.65 | +1.32 |
| cleanup_s | 0.34 | 0.18 | 3.45 | +3.27 |
| cold_tool_mean_ms |  | 51.18 | 105.81 | +54.63 |
| warm_tool_mean_ms |  | 51.05 | 105.86 | +54.80 |
| cold_harness_tool_mean_ms | 174.90 | 214.48 | 316.08 | +101.60 |
| warm_harness_tool_mean_ms | 167.70 | 200.22 | 303.86 | +103.64 |
| sandbox_cpu_mean | 4.25 | 3.37 | 1.02 | -2.35 |
| sandbox_cpu_peak | 12.62 | 24.43 | 11.21 | -13.22 |
| harness_cpu_peak | 193.70 | 207.79 | 230.21 | +22.42 |
| harness_memory_peak_mib | 926.93 | 1014.87 | 958.01 | -56.85 |

Per-run rows: tasks.csv
