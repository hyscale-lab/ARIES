# Model runtime implementation

This page describes the concrete mechanisms behind the
[model runtime contract](../design/runtime.md).

DeepSeek is supported only as an external OpenAI-compatible endpoint, with
bounded model validation. The `openai` backend names any other OpenAI-compatible
server, such as vLLM. It
describes the endpoint's API rather than a distinct runtime: it is external
only, has no native configuration file, adds no preparation step, and receives
the same bounded model discovery as external SGLang. SGLang may also be
external, or ARIES may manage one host process across a profile run.
Managed SGLang uses a native YAML file, explicit executable and timeouts, and
optional GPU indices; ARIES validates model, port, and GPU topology before side
effects. Service ownership and teardown follow the
[model runtime contract](../design/runtime.md#managed-service-operations).

Both direct run and the optional `aries setup PROFILE.json` command share one
idempotent lightweight preparation path for the pinned benchmark checkout,
selected task metadata, and exact Docker images. Direct run completes that
work before creating run artifacts, starting managed SGLang, checking model
health, or admitting tasks. Setup is prewarm-only: it validates backend
configuration and prepares those lightweight inputs, but never starts or stops
a runtime, contacts an external endpoint, installs SGLang, or downloads model
weights. Managed model loading and health therefore remain live, run-scoped
states rather than a persisted readiness marker.

The configured model credential is removed from the managed SGLang child
environment; logs remain private under the
[runtime isolation contract](../design/runtime.md#isolation-and-substitution).
ARIES does not install SGLang, download models, or configure connectivity between
the host endpoint and harness containers. See
the [quick start](../quick-start.md) for exact external and managed workflows.

Use HTTP model endpoints only as a trusted-local exception. Local
HTTP examples use a non-secret placeholder because ARIES requires a non-empty
credential value even for an unauthenticated server. Remote or credentialed
endpoints must use HTTPS; a real API key must never be sent over HTTP.

## Implementation gap: HTTP transport policy

The trusted-local restriction above is an operator requirement, not an enforced
URL-validation guarantee. [OpenAI-compatible URL normalization](../../pkg/model/sglang/client.go)
accepts HTTP on any host and its request builder attaches the configured Bearer
credential. [Profile validation](../../pkg/config/config.go) likewise accepts
HTTP(S) without enforcing locality. Consequently, a profile can send a real key
over remote HTTP despite the documented requirement. This documentation change
does not repair that code gap; use HTTPS for remote or credentialed endpoints.

## Source and evidence

[Runtime wiring](../../internal/app/wiring/runtime/sglang.go) constructs the
managed service; [SGLang configuration](../../internal/modelruntime/sglang/config.go)
validates native settings, and [process management](../../internal/modelruntime/sglang/process.go)
owns process startup and termination.
[Application preflight](../../internal/app/preflight.go) validates endpoints
before admission. [Runtime tests](../../internal/app/runtime_test.go) cover
health retry classification, exit races, and fresh cleanup contexts;
[process tests](../../internal/modelruntime/sglang/process_test.go) cover the
concrete managed process lifecycle. These references describe inspected source,
not a new model-server experiment.
