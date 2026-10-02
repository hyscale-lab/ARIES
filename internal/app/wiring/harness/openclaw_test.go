package harness

import (
	"github.com/hyscale-lab/aries/pkg/config"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
	"testing"
	"time"
)

func TestOpenClawVoiceOptionsSelectModeConfig(t *testing.T) {
	harness := config.HarnessConfig{
		Mode: openclawharness.ModeRealtime,
		Realtime: config.HarnessRealtimeConfig{
			ChunkDuration: time.Second,
			TTS:           config.RealtimeTTSConfig{Model: "realtime-tts"},
		},
	}
	options := openClawVoiceOptions(harness)
	if options.ChunkDuration != time.Second || options.TTS.Model != "realtime-tts" {
		t.Fatalf("realtime options = %#v", options)
	}

	harness.Mode = openclawharness.ModeVoiceTranscribe
	harness.VoiceTranscribe.HarnessRealtimeConfig = config.HarnessRealtimeConfig{
		ChunkDuration: 2 * time.Second,
		TTS:           config.RealtimeTTSConfig{Model: "voice-tts"},
	}
	options = openClawVoiceOptions(harness)
	if options.ChunkDuration != 2*time.Second || options.TTS.Model != "voice-tts" {
		t.Fatalf("voice-transcribe options = %#v", options)
	}
}
