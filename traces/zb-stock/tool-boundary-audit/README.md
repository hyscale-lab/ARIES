# Study 3 WildClawBench timing qualification

The four-arm campaign ran same-turn siblings concurrently. The published OpenClaw harness awaits each result before starting the next invocation. Of the 30 selected sources, 22 had multi-call turns. Each arm contains 45 such turns and 171 calls in those turns. The six order-6 repeats use the same scheduling rule. Archived times and VM states describe the ARIES concurrent schedule. A serial replay is required to measure the original harness behavior for these workloads.

The native local prefetch operation can continue after first dispatch. This occurred in 17 of 260 WildClawBench timed restores and 66 of 301 TB timed restores. Harness restore latency measures the blocking restore call. [Affected source counts](source-coverage.csv)
