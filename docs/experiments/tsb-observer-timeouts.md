# Observer timeout allowance

The recovered access/dirty suite optionally applies a task-local observer timeout
allowance to `rstan-to-pystan`: metadata/publication waits of 300 seconds, HTTP
waits of 900 seconds, and hook/child waits of 905 seconds. Other tasks retain
their observer limits. This allowance changes observer waiting only; native agent
and verifier budgets are separately recorded. The suite's default agent override
for RStan is 5400 seconds; an explicit empty `agent_timeout_overrides` object
keeps every native agent deadline. See [access/dirty configuration](tsb-access-dirty.md#preparation-and-gates).

Preparation pins the four task-local plugin files and observer configuration.
Admission verifies those hashes before setup and execution. Cancellation, child
reaping and capture-discard behavior follow the
[VMM-owned backend contract](tsb-access-dirty.md#vmm-owned-tracing-backend). A
longer wait never certifies successful capture, terminal completion, or low
overhead.
