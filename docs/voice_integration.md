# OpenClaw Voice Integration

## Changes

- Added `harness.mode` to select between the existing OpenClaw `agent` path and
  the new `realtime` path. `agent` remains the default when the field is absent.
- Added `harness.realtime` profile settings for TTS, gateway session options,
  stream timing, tool-call waiting, trailing silence, and optional raw event
  retention.
- Added a shared host-side OpenClaw Gateway client. Both normal text-agent and
  realtime modes dial the same container-published WebSocket endpoint and
  authenticate with the per-run gateway token.
- Normal text-agent instructions now use the pinned v2026.7.1 `agent` Gateway
  operation rather than `docker exec`. ARIES sends one request with one stable
  per-run idempotency key, requires an accepted response and terminal response
  with the same frame ID and non-empty run ID, and never resubmits after send.
- Moved talk/chat/tool parsing and the host-side realtime runner into the
  explicit `pkg/harness/openclaw/realtime` package. It creates the talk session, streams audio,
  processes transcript/output/chat/tool events, waits for nested agent runs, and
  writes a structured realtime result.
- Added audio preprocessing utilities for WAV PCM16 input, silence generation,
  sample-rate conversion, PCM16 output, and G.711 u-law output.
- Added realtime tool-call bridging: `openclaw_agent_consult` starts a nested
  OpenClaw agent run through the gateway, and `openclaw_agent_control` is handled
  locally for runner status.
- Agent and realtime modes publish exactly one ephemeral gateway port on host
  `127.0.0.1`. The authenticated container-local relay targets only the
  loopback-bound OpenClaw gateway; missing, duplicate, wildcard, or invalid
  Docker bindings fail startup.
- Added private staging for an optional realtime/TTS API key, separate from the
  model API key and gateway token.

## Running

Set `OPENAI_API_KEY` for the TTS request in addition to the configured model
key, then run the realtime profile:

```sh
./bin/aries profiles/openclaw-tb2-fix-git-realtime-deepseek.json
```

## Artifacts

New realtime-specific artifacts:

- `harness/voice-instruction.txt`: the exact task instruction text used as TTS
  input.
- `harness/voice-instruction.wav`: the generated WAV that is streamed into the
  realtime session after preprocessing.
- `harness/voice-instruction.wav.meta.json`: metadata for the generated WAV,
  including provider, model, voice, format, input text hash, input character
  count, cache flag, and output path.
- `harness/realtime-result.json`: structured realtime runner output, including
  transcript text, output text, tool-call counts, agent run IDs, event counts,
  session IDs, errors, and optional raw events when configured.

Text mode writes `harness/agent-result.json`. It contains only the sanitized
connected role and sorted scopes, the correlated run ID, the final response,
and a redacted failure diagnostic when applicable. Challenge nonces, gateway
tokens, device tokens, signatures, raw authentication frames, and idempotency
keys are not retained.

## Package boundaries

- `openclaw.Manager` owns the OpenClaw container lifecycle and loopback port.
- `openclaw/gateway.Client` owns one authenticated protocol connection,
  request/event dispatch, and the agent accepted-to-terminal contract.
- `openclaw/realtime.Runner` owns one voice/talk session and its semantic
  talk/chat/tool events.

Realtime requires both `operator.read` and `operator.write`; text-agent mode
requires only `operator.write`. Event queue or diagnostic-history overflow is
terminal and closes the connection instead of silently dropping frames.
Unmatched, late, duplicate, and missing-ID response frames are discarded rather
than entering event history, and authentication challenges are accepted only
during the active handshake. Protocol errors retain only bounded code/message
metadata; raw response and authentication payloads are never rendered.

Host audio processing accepts sample rates through 384 kHz and bounds PCM16
input/output to 32 MiB, WAV input to 33 MiB, and generated trailing silence to
60 seconds. Inputs whose resampled output would exceed the bound fail before
allocation.
