# ARIES

ARIES runs agent benchmarks reproducibly and observes the whole trajectory. This glossary fixes
the terms for the tool bridge, the access path from an agent harness to its task sandbox.

## Language

**Tool bridge**:
The run-shared machinery that gives one harness temporary, narrow access to one sandbox and
revokes it before evaluation. It is the same for every transport.
_Avoid_: bridge process, per-task bridge

**Control plane**:
The Runner-to-bridge API that registers, inspects and releases a sandbox.
_Avoid_: bridge gRPC (gRPC is also a transport)

**Transport**:
The interchangeable wire between a harness and the tool bridge for tool calls: SSH or gRPC. One
transport serves a whole run.
_Avoid_: route, protocol (when the transport is meant)

**Dialect**:
The command grammar one harness speaks for tool calls over SSH. The gRPC transport has typed
calls and no dialect.
_Avoid_: wire grammar

**Tool-call record**:
One structured line of `tool-calls.jsonl` per tool call. Its format is the same for every
transport.

**Raw log**:
The opt-in, private, lossless record of request bytes for one transport (`<transport>_raw.log`).
It keeps commands, stdin and written content, never output or read content.

## Relationships

- A **Run** has exactly one **Tool bridge** and exactly one **Transport**.
- A **Tool bridge** registers many sandboxes over the **Control plane**, one per task.
- A **Harness** speaks one **Dialect** over SSH, or typed calls over gRPC.
- A **Transport** writes **Tool-call records** and, when the profile opts in, a **Raw log**.

## Flagged ambiguities

- "gRPC bridge" meant both the control plane and the gRPC transport. Resolved: the control plane
  uses gRPC internally; "gRPC" alone names the transport.
