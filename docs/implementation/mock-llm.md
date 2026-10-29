# Mock LLM smoke experiments

`make build` produces `bin/aries-echo`, the deterministic OpenAI-compatible
service from `feat/mock-llm-serving`. It needs no model weights or paid API.
It serves `/v1/models` and streaming or nonstreaming `/v1/chat/completions`.
The default response echoes the received request, with credential headers
redacted. `-log PATH` appends those request records as private JSONL evidence.

Use `-tool-command` with OpenClaw to issue one native `exec` tool call, then
finish with the matching tool result:

```sh
make build
./bin/aries-echo -addr 0.0.0.0:18080 \
  -log .cache/mock-requests.jsonl \
  -tool-command 'printf "ARIES_MICROVM_TOOL\n"; uname -r; cat /proc/sys/kernel/random/boot_id; cat /proc/cmdline; pwd'
```

In another terminal, on a [prepared vHive worker](vhive.md):

```sh
ECHO_API_KEY=mock-only ./bin/aries profiles/vhive/openclaw-tb2-fix-git-echo-vhive.json
```

For a suspend/resume smoke experiment, select
`sandbox.deployment.vhive.idle_mode: "checkpoint"` and provide a second command.
The mock waits for the first tool result before requesting the second call,
then returns both actual results in its final answer:

```sh
./bin/aries-echo -addr 0.0.0.0:18080 \
  -log .cache/mock-checkpoint-requests.jsonl \
  -tool-yield-ms 300000 \
  -tool-command 'cat /proc/sys/kernel/random/boot_id; sleep 600 >/tmp/aries-background.log 2>&1 </dev/null & echo $! >/tmp/aries-background.pid; cat /tmp/aries-background.pid; printf "preserved\n" >/tmp/aries-checkpoint-marker' \
  -tool-followup-command 'cat /proc/sys/kernel/random/boot_id; cat /tmp/aries-background.pid; kill -0 "$(cat /tmp/aries-background.pid)" && echo PROCESS_ALIVE; cat /tmp/aries-checkpoint-marker'
```

Compare the boot IDs and saved PID, confirm `PROCESS_ALIVE` and the marker,
and check the worker lifecycle evidence for VMM removal and restoration between
calls. The filesystem probe alone cannot establish that VM memory was restored.
`-tool-followup-command` requires `-tool-command`; omitting it preserves the
single-call behavior.

`-tool-yield-ms` forwards the numeric OpenClaw `exec.yieldMs` argument so slow
checkpoint operations can finish before native asynchronous backgrounding.
Zero (the default) omits the field and preserves OpenClaw's native wait default.
Positive values require `-tool-command`; OpenClaw applies its own native limits.

The example selects the existing `openai` external backend and model
`aries-echo`. Its `http://172.17.0.1:18080/v1` URL is the usual native Docker host
bridge address. Set `model.base_url` to an address reachable by both Runner and
the harness if your worker uses a different network layout. The key is a local
placeholder; no provider credential is needed. The worker's socket, devmapper
capacity and CNI settings still need to match its deployment profile.

Tool-command mode requires the offered OpenClaw `exec` function, whose argument
is `{"command": "..."}`. It preserves the supplied shell text. The exchange is
derived from request history, so parallel conversations do not share execution
state. Each command and user turn gets a distinct alphanumeric call ID; this survives
OpenClaw's ID normalization and lets the matching tool result terminate the
turn. Other harness tool schemas are not implemented by this mode.

To verify a microVM run, inspect the harness's `agent-result.json`, bridge tool
records and `run-result.json`. Compare the reported guest kernel and
`/proc/sys/kernel/random/boot_id` with the host; the guest command line should
also identify its VM root device. Check harness completion, confirmed isolation,
evaluation and cleanup separately. This diagnostic command does not solve the
benchmark task, so a reward of `0` is expected. Shared bridge observation should
still succeed; VM resource observation remains explicitly unsupported.

Unit tests exercise streaming and nonstreaming tool exchanges, credential-header
redaction and native call-ID normalization. A real-worker smoke experiment has
also exercised OpenClaw → SSH bridge → containerd → Firecracker guest, including
tool-result return, independent benchmark evaluation and successful cleanup.
