# Tool bridge alternatives

## Decision

Keep the current pair-specific OpenClaw bridge:

```text
OpenClaw SSH backend -> ARIES SSH endpoint -> Sandbox.ExecStream
```

It is not the smallest possible transport implementation, but it is the
smallest one that uses OpenClaw's unmodified upstream integration point while
keeping the Docker socket out of the harness and task-container lifecycle under
ARIES. The bridge should remain concrete; ARIES should not add one universal
remote-tool protocol.

## Harness comparison

| Harness | Upstream remote execution boundary | Recommended ARIES adapter |
| --- | --- | --- |
| OpenClaw | Native SSH sandbox backend and canonical remote workspace | Retain `openclawssh`; terminate SSH and translate accepted exec/file operations to the supplied sandbox. |
| Hermes | Separate SSH and Docker environment implementations | Add a Hermes-specific SSH adapter only when Hermes is implemented. Its SCP, ControlMaster, remote-home, and sync behavior differs from OpenClaw. |
| OpenHands | `Workspace`/`RemoteWorkspace` backed by agent-server HTTP/WebSocket | Adapt the OpenHands workspace API to `Exec`, `Upload`, and `Download`, or run a controlled agent-server sidecar. Do not force OpenHands through SSH. |

Using each harness's own Docker backend is not appropriate: it would make the
harness create or own a second container rather than modify the exact ARIES
sandbox later inspected by the evaluator. Putting `sshd` in every task image
would also fail because many Terminal-Bench images do not provide it and would
pollute the evaluated environment.

## Shared boundary

Only the Runner-facing `ToolBridge` interface should be shared. Concrete
adapters may reuse small encoding or evidence helpers after a second real use
appears, but their authentication, workspace, transfer, cancellation, and
revocation semantics should remain harness-specific.

## Upstream references

- OpenClaw [sandbox documentation](https://github.com/openclaw/openclaw/blob/main/docs/gateway/sandboxing.md),
  [SSH transport](https://github.com/openclaw/openclaw/blob/main/src/agents/sandbox/ssh.ts),
  and [SSH backend](https://github.com/openclaw/openclaw/blob/main/src/agents/sandbox/ssh-backend.ts).
- Hermes [configuration](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/configuration.md),
  [SSH environment](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/ssh.py),
  and [Docker environment](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/docker.py).
- OpenHands [Workspace architecture](https://docs.openhands.dev/sdk/arch/workspace),
  [agent-server overview](https://docs.openhands.dev/sdk/guides/agent-server/overview),
  and [Docker sandbox](https://docs.openhands.dev/openhands/usage/sandboxes/docker).
