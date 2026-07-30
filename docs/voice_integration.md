# OpenClaw Voice Integration

## Changes

- Added `harness.mode` to select between the existing OpenClaw `agent` path and
  the new `realtime` path. `agent` remains the default when the field is absent.
- Added `harness.realtime` profile settings for TTS, gateway session options,
  stream timing, tool-call waiting, trailing silence, and optional raw event
  retention.
- Added a host-side OpenClaw Gateway client: ARIES dials the container-published
  websocket endpoint, authenticates with the gateway token, creates realtime
  sessions, sends requests, and receives events.
- Added a host-side realtime runner: it creates the talk session, streams audio,
  processes transcript/output/chat/tool events, waits for nested agent runs, and
  writes a structured realtime result.
- Added audio preprocessing utilities for WAV PCM16 input, silence generation,
  sample-rate conversion, PCM16 output, and G.711 u-law output.
- Added realtime tool-call bridging: `openclaw_agent_consult` starts a nested
  OpenClaw agent run through the gateway, and `openclaw_agent_control` is handled
  locally for runner status.
- Added container runtime support for realtime mode: the OpenClaw container
  starts a realtime gateway script, exposes the gateway on host loopback, and
  keeps the existing SSH bridge and sandbox lifecycle.
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
