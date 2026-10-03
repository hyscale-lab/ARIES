package harness

import (
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	hermesharness "github.com/hyscale-lab/aries/pkg/harness/hermes"
	"github.com/sirupsen/logrus"
)

// NewHermes takes ownership of transport, including closing it on construction failure.
func NewHermes(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger, transport deployment.Deployment) (app.HarnessInstance, error) {
	if err := ValidateMCPServers(cfg.Harness); err != nil {
		return app.HarnessInstance{}, errors.Join(err, transport.Close())
	}
	options := hermesharness.Options{
		Deployment: transport, Image: cfg.Versions.Hermes.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger,
		Mode: cfg.Harness.Mode, WebSearchEnabled: cfg.Harness.WebSearch.Enabled,
		ExtractAPIKeyEnv:       cfg.Harness.WebSearch.ExtractAPIKeyEnv,
		SubagentsEnabled:       cfg.Harness.Subagents.Enabled != nil && *cfg.Harness.Subagents.Enabled,
		MaxConcurrentSubagents: cfg.Harness.Subagents.MaxConcurrent,
		Compaction:             hermesCompaction(cfg.Harness.Compaction),
		ExtraBody:              hermesExtraBody(cfg.Harness.Hermes),
		MCPServers:             cfg.Harness.MCPServers,
	}

	if cfg.Harness.Mode == hermesharness.ModeVoiceTranscribe {
		options.VoiceTranscribe = hermesVoiceOptions(cfg.Harness.VoiceTranscribe)
	}
	manager, err := hermesharness.New(options)
	if err != nil {
		return app.HarnessInstance{}, errors.Join(fmt.Errorf("construct Hermes harness: %w", err), transport.Close())
	}
	return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
}

func hermesVoiceOptions(voice config.HarnessVoiceTranscribeConfig) hermesharness.VoiceTranscribeOptions {
	return hermesharness.VoiceTranscribeOptions{
		TTS: hermesharness.VoiceTTSOptions{
			Provider: voice.TTS.Provider, BaseURL: voice.TTS.BaseURL,
			APIKeyEnv: voice.TTS.APIKeyEnv, Model: voice.TTS.Model,
			Voice: voice.TTS.Voice, Instructions: voice.TTS.Instructions,
			Speed: voice.TTS.Speed, Timeout: voice.TTS.Timeout,
		},
		STT: hermesharness.VoiceSTTOptions{
			Provider: voice.STT.Provider, Model: voice.STT.Model,
			Language: voice.STT.Language, Timeout: voice.STT.Timeout,
		},
	}
}

func hermesExtraBody(block *config.HarnessHermesConfig) []byte {
	if block == nil {
		return nil
	}
	return []byte(block.ExtraBody)
}

func hermesCompaction(block *config.HarnessCompactionConfig) *hermesharness.CompactionSettings {
	if block == nil {
		return nil
	}
	return &hermesharness.CompactionSettings{Enabled: block.Enabled, ThresholdTokens: block.ThresholdTokens}
}
