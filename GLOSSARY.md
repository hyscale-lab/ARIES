# ARIES language

These terms name the sandbox-call replay concepts. The [tool bridge terms](docs/design/bridge.md#terms) define tool calls and sandbox calls.

## Language

**Logical task**:
A benchmark case selected by its catalog ID, before a run assigns an execution ID.
_Avoid_: Task occurrence.

**Task occurrence**:
One execution of a logical task, with its own run-local ID, sandbox, and call log.
_Avoid_: Logical task.

**Sandbox-call replay**:
A task execution that repeats recorded sandbox calls without an agent harness or agent model.
_Avoid_: Task-arrival replay.

**Replay trace**:
The recorded requests, expected responses, and timing of sandbox calls from one task occurrence.
_Avoid_: Arrival trace.

**Replay-ready trace**:
A replay trace with complete request inputs, response checks, and timing.
_Avoid_: Complete log.

**Replay mismatch**:
A difference between an expected sandbox-call response and the response to its replayed request.
_Avoid_: Benchmark failure.
