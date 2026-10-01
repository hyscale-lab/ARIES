# Mock LLM smoke experiments

`make build` produces `bin/aries-echo`, a deterministic OpenAI-compatible
service requiring no model weights or paid API. It serves `/v1/models` and
streaming or nonstreaming `/v1/chat/completions`. By default it echoes the
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
