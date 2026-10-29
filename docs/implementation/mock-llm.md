# Mock LLM smoke experiments

`make build` produces `bin/aries-echo`, a deterministic OpenAI-compatible
service requiring no model weights or paid API. It serves `/v1/models` and
streaming or non-streaming `/v1/chat/completions`. By default it echoes the
received request, with credential headers redacted. `-log PATH` appends those
request records as private JSONL evidence.

```sh
make bridge-image
./bin/aries-echo -addr 0.0.0.0:8080 -log .cache/mock-requests.jsonl
```

In another terminal:

```sh
ECHO_API_KEY=mock-only ./bin/aries profiles/openclaw-tb2-fix-git-echo.json
# Or use profiles/hermes-tb2-fix-git-echo.json.
```

The profiles select the `openai` external backend and model `aries-echo`.
Their `http://172.17.0.1:8080/v1` URL uses the usual native Docker host bridge
address. Set `model.base_url` to an address reachable by both Runner and the
harness if your Docker network uses a different address. The server must listen
on that interface; a loopback-only listener cannot receive container requests.
The key is a local placeholder; no provider credential is needed.

Inspect the harness response and request log to see the actual tool definitions,
message history and serving parameters. Echoing a request does not solve the
benchmark task; a reward of `0` is expected. Inspect harness completion,
evaluation, isolation and cleanup separately in `run-result.json`.

## Tool calls with OpenClaw

The `-tool-command` option is **experimental**. Use it to issue one native `exec`
call per user turn, then finish with the matching tool result. For example,
start the server as follows and run the OpenClaw echo profile above:

```sh
./bin/aries-echo -addr 0.0.0.0:8080 \
  -log .cache/mock-requests.jsonl \
  -tool-command 'printf "ARIES_TOOL\n"; uname -r; pwd'
```

Add `-tool-followup-command` to issue a second `exec` after the first result,
then return both results in the final answer:

```sh
./bin/aries-echo -addr 0.0.0.0:8080 \
  -tool-command 'printf "preserved\n" >/tmp/aries-mock-marker' \
  -tool-followup-command 'cat /tmp/aries-mock-marker'
```

The follow-up option requires `-tool-command`. `-tool-yield-ms` forwards the
numeric OpenClaw `exec.yieldMs` argument. Zero (the default) omits it and
preserves native behavior for running commands in the background; positive
values require `-tool-command`, and OpenClaw applies its own native limits.

Tool-command mode requires an offered OpenClaw `exec` function accepting
`{"command": "..."}`. It preserves the supplied shell text. Each command and
user turn gets a distinct alphanumeric call ID that survives OpenClaw's native
ID normalization. Request history determines the next response, so parallel
conversations do not share execution state. Other harness tool schemas are
not implemented by this mode; use default echo mode with Hermes.

Unit tests cover streaming and non-streaming tool exchanges, follow-up calls,
request replay, separate user turns, credential redaction and option validation.
