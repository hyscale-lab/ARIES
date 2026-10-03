package harness

import (
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
	"github.com/sirupsen/logrus"
)

// NewOpenClaw takes ownership of transport, including closing it on construction failure.
func NewOpenClaw(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger, transport deployment.Deployment) (app.HarnessInstance, error) {
	if err := ValidateMCPServers(cfg.Harness); err != nil {
		return app.HarnessInstance{}, errors.Join(err, transport.Close())
	}
	options := openclawharness.Options{
		Deployment: transport, Image: cfg.Versions.OpenClaw.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger,
		Mode: cfg.Harness.Mode, WebSearchEnabled: cfg.Harness.WebSearch.Enabled,
		ExtractAPIKeyEnv:       cfg.Harness.WebSearch.ExtractAPIKeyEnv,
		SubagentsEnabled:       cfg.Harness.Subagents.Enabled != nil && *cfg.Harness.Subagents.Enabled,
		MaxConcurrentSubagents: cfg.Harness.Subagents.MaxConcurrent,
		MCPServers:             cfg.Harness.MCPServers,
	}
	if cfg.Harness.Mode == openclawharness.ModeRealtime || cfg.Harness.Mode == openclawharness.ModeVoiceTranscribe {
		options.Realtime = openClawVoiceOptions(cfg.Harness)
	}
	manager, err := openclawharness.New(options)
	if err != nil {
		return app.HarnessInstance{}, errors.Join(fmt.Errorf("construct OpenClaw harness: %w", err), transport.Close())
	}
	return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
}

func openClawVoiceOptions(harness config.HarnessConfig) openclawharness.RealtimeOptions {
	realtime := harness.Realtime
	if harness.Mode == openclawharness.ModeVoiceTranscribe {
		realtime = harness.VoiceTranscribe.HarnessRealtimeConfig
	}
	return openclawharness.RealtimeOptions{
		AgentQuestionTemplate: realtime.AgentQuestionTemplate,
		TTS: openclawharness.RealtimeTTSOptions{
			Provider: realtime.TTS.Provider, BaseURL: realtime.TTS.BaseURL,
			APIKeyEnv: realtime.TTS.APIKeyEnv, Model: realtime.TTS.Model,
			Voice: realtime.TTS.Voice, Instructions: realtime.TTS.Instructions,
			Speed: realtime.TTS.Speed, Timeout: realtime.TTS.Timeout,
		},
		ChunkDuration:         realtime.ChunkDuration,
		ListenDuration:        realtime.ListenDuration,
		QuietDuration:         realtime.QuietDuration,
		AgentWaitDuration:     realtime.AgentWaitDuration,
		ToolCallTimeout:       realtime.ToolCallTimeout,
		TrailingSilenceMillis: realtime.TrailingSilenceMillis,
		Provider:              realtime.Provider,
		Model:                 realtime.Model,
		Voice:                 realtime.Voice,
		ReasoningEffort:       realtime.ReasoningEffort,
		IncludeEvents:         realtime.IncludeEvents,
	}
}
