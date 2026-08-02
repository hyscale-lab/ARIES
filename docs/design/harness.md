# AgentHarness

`AgentHarness` owns agent execution. It receives a task instruction, model
configuration, and temporary bridge connection details, then starts the agent,
runs it within the configured deadline, and stops it positively before bridge
revocation or evaluation can proceed.

## Boundary and lifecycle

The harness does not own the benchmark task container, verifier material,
bridge listener, or evaluation. It may retain its own logs, telemetry, and
rendered configuration as private run artifacts. Model credentials are supplied
at runtime and must not be written into profiles, structured logs, Docker
metadata, or results.

The current implementation is OpenClaw in a pinned container image. ARIES starts
the harness only after the sandbox and bridge are ready. On success, failure, or
cancellation, it stops the harness and confirms absence. Evaluation remains a
separate Benchmark outcome rather than an interpretation of harness success.

OpenClaw container lifecycle, gateway protocol, and voice-session semantics are
separate concrete responsibilities. `openclaw.Manager` owns the container and
publishes one ephemeral host-loopback port. `openclaw/gateway.Client` owns one
authenticated WebSocket protocol connection and bounded fail-closed frame
dispatch. `openclaw/realtime.Runner` consumes that client for talk/chat/tool
session semantics; the gateway package intentionally contains no voice event
parsers or Docker lifecycle.

Text and realtime modes share the authenticated Gateway transport. Text sends
exactly one pinned-version `agent` request and correlates accepted and terminal
responses by both frame request ID and non-empty run ID; an ambiguous send,
disconnect, timeout, or protocol mismatch is never retried. Realtime requires
read and write scopes, while text requires write scope. Only sanitized role and
sorted scope metadata may leave the authentication boundary.

Realtime converts the task instruction to staged audio, owns one talk session,
and may invoke nested agent runs through the same authenticated client. Its
audio, transcript, result, and optional event records remain private harness
artifacts. The separate realtime/TTS credential is staged privately for voice
mode and is not part of model configuration, Gateway authentication, or
structured results.
Both modes remain concrete behavior of the single `AgentHarness` role; realtime
does not create a fifth Runner role or take ownership from the benchmark,
sandbox, or bridge.

## Customization & Contribution Guide

Add a harness only when it can implement the existing `AgentHarness` lifecycle
without taking ownership from the other roles. Keep the implementation in a
concrete harness package with an explicit constructor, add its explicit switch
to the command wiring, and provide focused tests for start, run, cancellation,
idempotent stop, positive absence, credential handling, and artifacts. Update
the supported reference and this guide with evidenced behavior; do not add a
registration, discovery, factory, reflection, DI, or generic plugin layer.
