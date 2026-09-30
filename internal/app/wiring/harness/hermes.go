package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
	hermesharness "github.com/hyscale-lab/aries/pkg/harness/hermes"
	"github.com/sirupsen/logrus"
)

// NewHermes takes ownership of transport, including closing it on construction failure.
func NewHermes(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger, transport deployment.Deployment) (app.HarnessInstance, error) {
	if err := ValidateMCPServers(cfg.Harness); err != nil {
		return app.HarnessInstance{}, errors.Join(err, transport.Close())
	}
	image, err := hermesharness.LocalImage(cfg.Versions.Hermes.Image, hermesOTelPlugin(cfg))
	if err != nil {
		return app.HarnessInstance{}, errors.Join(err, transport.Close())
	}
	options := hermesharness.Options{
		Runtime: harnesscommon.RuntimeOptions{
			Deployment:   transport,
			Image:        image,
			OutputDir:    outputRoot,
			APIKeyLookup: lookup,
			Logger:       logger,
		},
		Common:     commonOptions(cfg),
		Compaction: hermesCompaction(cfg.Harness.Compaction),
		ExtraBody:  hermesExtraBody(cfg.Harness.Hermes),
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

// PrepareHermesImage builds the image Hermes runs, the pinned one plus
// hermes-otel, through the deployment's build-if-missing operation.
func PrepareHermesImage(ctx context.Context, cfg config.Config, build func(ctx context.Context, image, dockerfile string, buildArgs map[string]string) error) error {
	plugin := hermesOTelPlugin(cfg)
	image, err := hermesharness.LocalImage(cfg.Versions.Hermes.Image, plugin)
	if err != nil {
		return err
	}
	args, err := hermesharness.OTelBuildArgs(cfg.Versions.Hermes.Image, plugin)
	if err != nil {
		return err
	}
	return build(ctx, image, hermesharness.OTelDockerfile, args)
}

func hermesOTelPlugin(cfg config.Config) hermesharness.OTelPlugin {
	pin := cfg.Versions.Hermes.OTelPlugin
	return hermesharness.OTelPlugin{RepositoryURL: pin.RepositoryURL, Version: pin.Version, Revision: pin.Revision}
}

func hermesVoiceOptions(voice config.HarnessVoiceTranscribeConfig) hermesharness.VoiceTranscribeOptions {
	return hermesharness.VoiceTranscribeOptions{
		TTS: harnesscommon.TTSOptions{
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
