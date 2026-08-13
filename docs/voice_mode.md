# Voice Modes

Three voice-oriented harness modes are currently supported. In each voice mode, the task prompt is synthesized into `voice-instruction.wav` at the start of the task run.

## OpenClaw `realtime`

Use:

```json
"harness": {
  "type": "openclaw",
  "mode": "realtime",
  "realtime": {
    "provider": "openai",
    "model": "gpt-realtime",
    "tts": {
      "provider": "openai",
      "api_key_env": "OPENAI_API_KEY"
    }
  }
}
```

This is OpenClaw’s realtime Talk mode. Synthesized audio chunks are streamed to the OpenClaw Gateway, which relays them to the realtime voice provider. The recognized request is then always forwarded to the OpenClaw agent via `agent-consult`, as `force-agent-consult` is enabled in our setup. Rather than providing a direct transcription, the realtime provider generates a request for the downstream agent. Consequently, the original spoken request may be reformulated, summarized, or transformed into an agent-oriented instruction before reaching the agent.

For OpenClaw, the ASR/realtime provider and model are the top-level
`harness.realtime.provider` and `harness.realtime.model` fields; they are sent
as gateway session parameters. `harness.realtime.tts` configures only
synthesis of the task prompt into WAV before the task run starts.

Main artifacts:

- `harness/voice-instruction.txt` - text used to synthesize the task audio.
- `harness/voice-instruction.wav` - synthesized task audio.
- `harness/voice-instruction.wav.meta.json` - TTS provider/model/voice metadata.
- `harness/realtime-result.json` - realtime session result, transcript, events
  when enabled, tool calls, and errors.

## OpenClaw `voice-transcribe`

Use:

```json
"harness": {
  "type": "openclaw",
  "mode": "voice-transcribe",
  "voice_transcribe": {
    "provider": "openai",
    "model": "gpt-realtime",
    "tts": {
      "provider": "openai",
      "api_key_env": "OPENAI_API_KEY"
    }
  }
}
```

This mode uses OpenClaw’s realtime transcription mode. Synthesized audio chunks are streamed to the OpenClaw Gateway, which performs streaming speech recognition and returns the resulting transcript without generating a conversational response. The transcript is then passed to the OpenClaw agent as a regular text input, reproducing the default text-based pipeline.

Main artifacts are the same as in `realtime` mode.

## Hermes `voice-transcribe`

Use:

```json
"harness": {
  "type": "hermes",
  "mode": "voice-transcribe",
  "voice_transcribe": {
    "tts": {
      "provider": "openai",
      "api_key_env": "OPENAI_API_KEY"
    },
    "stt": {
      "provider": "openai",
      "model": "gpt-4o-mini-transcribe"
    }
  }
}
```

This mode follows Hermes’ voice interaction pipeline. The synthesized audio is passed as a complete utterance to the configured Hermes ASR engine for transcription. The resulting transcript is then used as input to a Hermes agent run.

Main artifacts:

- `harness/voice-instruction.txt` - text used to synthesize the task audio.
- `harness/voice-instruction.wav` - synthesized task audio.
- `harness/voice-instruction.wav.meta.json` - TTS provider/model/voice metadata.
- `harness/voice-transcript.txt` - final transcript passed to the agent.
- `harness/voice-result.json` - STT result, transcript, agent input, and agent
  output.
