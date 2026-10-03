# Architecture

## Code ownership

| Change | Location |
| --- | --- |
| CLI grammar and implementation switches | `cmd/aries` |
| Profile loading, preparation, scheduling, results | `internal/app` |
| Configuration-to-constructor mapping and partial-construction cleanup | `internal/app/wiring/<role>/<implementation>.go` |
| Four role interfaces and task lifecycle | `pkg/runner` |
| Small shared data | `pkg/core` |
| Concrete behavior | `pkg/benchmark`, `pkg/harness`, `pkg/bridge`, `pkg/sandbox` |
| Shared deployment contract and Docker provider | `pkg/deployment`, `pkg/deployment/docker` |

`cmd` injects selected constructors through `app.Wiring`. `internal/app` must not
import its wiring subpackages. Keep selection out of wiring helpers; share
benchmark constructors between preparation and execution. Harness and sandbox
consume the neutral deployment contract, without importing the Docker provider.

## Task lifecycle

Load task → start sandbox → benchmark sanitizes sandbox → start bridge → start/run
harness → positively stop harness → positively revoke bridge → benchmark evaluates
the same live sandbox → remove sandbox container → remove owned network.

Failed isolation gates block verifier exposure and evaluation. Harness outcome
and evaluation outcome are separate. Cleanup covers partial starts, runs in
reverse ownership order, and uses fresh bounded contexts after cancellation.
Closing a transport does not prove its resources are gone. Use `context.Context`
for external work, idempotent stop methods, and Logrus lifecycle logging.
Keep helpers concrete and package-private unless Runner substitutes them;
prefer existing patterns. Add dependencies only when the standard library and
existing dependencies are insufficient.

Each admitted occurrence gets fresh components, identity, and network, including
repeated task IDs. Concurrency bounds admissions; admitted work drains through
cleanup. Managed model runtimes and monitoring surround Runner, adding no roles.

Human reference: [architecture](../docs/design.md).
