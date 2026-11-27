package harness

import (
	"encoding/json"
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

func TestOpenClawBlockReachesHarnessOptions(t *testing.T) {
	block := &config.HarnessOpenClawConfig{
		Model:             json.RawMessage(`{"contextWindow":2000000}`),
		Provider:          json.RawMessage(`{"timeoutSeconds":900}`),
		AgentDefaults:     json.RawMessage(`{"heartbeat":{"every":"0m"}}`),
		TelemetryLimitMiB: 256,
	}
	overlay, limit, err := openClawOverlay(block)
	if err != nil || limit != 256<<20 {
		t.Fatalf("limit = %d, %v", limit, err)
	}
	if overlay.Model["contextWindow"] != json.Number("2000000") || overlay.Provider["timeoutSeconds"] != json.Number("900") || overlay.AgentDefaults["heartbeat"] == nil {
		t.Fatalf("overlay = %#v", overlay)
	}
	overlay, limit, err = openClawOverlay(nil)
	if err != nil || limit != 0 || overlay.Model != nil || overlay.Provider != nil || overlay.AgentDefaults != nil {
		t.Fatalf("absent block = %#v, %d, %v", overlay, limit, err)
	}
}
