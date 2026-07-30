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

## Customization & Contribution Guide

Add a harness only when it can implement the existing `AgentHarness` lifecycle
without taking ownership from the other roles. Keep the implementation in a
concrete harness package with an explicit constructor, add its explicit switch
to the command wiring, and provide focused tests for start, run, cancellation,
idempotent stop, positive absence, credential handling, and artifacts. Update
the supported reference and this guide with evidenced behavior; do not add a
registration, discovery, factory, reflection, DI, or generic plugin layer.
