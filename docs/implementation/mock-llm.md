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

The example selects the existing `openai` external backend and model
`aries-echo`. Its `http://172.17.0.1:18080/v1` URL is the usual native Docker host
bridge address. Set `model.base_url` to an address reachable by both Runner and
the harness if your worker uses a different network layout. The key is a local
placeholder; no provider credential is needed. The worker's socket, devmapper
capacity and CNI settings still need to match its deployment profile.

Tool-command mode requires the offered OpenClaw `exec` function, whose argument
is `{"command": "..."}`. It preserves the supplied shell text. The exchange is
derived from request history, so parallel conversations do not share execution
state. Each user turn gets a distinct alphanumeric call ID; this survives
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
