# ARIES microbenchmark tasks

Five Terminal-Bench-format tasks that each isolate one cost of running an agent
against a tool sandbox, for comparing deployment backends. Every instruction
names the exact commands to run, so a model takes the same path each time, and
recording one run with `aries-replay` and replaying it fixes the model's output
and latency across backends.

| Task | The agent | The verifier checks | Isolates |
| --- | --- | --- | --- |
| `mb-noop` | replies without using a tool | nothing; always passes | harness, model and lifecycle cost |
| `mb-one-call` | runs `touch /tmp/mb-one` | the file exists | one tool round trip |
| `mb-ten-calls` | runs ten `echo` commands, one tool call each | the file holds lines 1 to 10 | the cost per call |
| `mb-cpu` | runs a fixed 3,000,000-step SHA-256 chain | the final hash | a CPU-bound call, about 2.5 s on a 2.4 GHz core |
| `mb-output` | prints 66,399 bytes of `base64` | the saved copy's size | output carried back through the bridge |

All tasks use the public `python:3.12.12-slim-bookworm` image, so a cluster
pulls it like any other task image, and need no network. The verifiers write
the reward and CTRF report with plain shell.

ARIES runs these tasks from a packed checkout: see
[the microbenchmark profiles](../../docs/configuration.md#microbenchmark-profiles).
