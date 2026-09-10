package audio

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

const (
	VoiceInstructionTextFile = "voice-instruction.txt"
	VoiceInstructionWAVFile  = "voice-instruction.wav"
	VoiceInstructionMetaFile = "voice-instruction.wav.meta.json"
)

type SpeechSynthesizer interface {
	Synthesize(context.Context, SpeechRequest) (SpeechResult, error)
	Close()
}

type NewSpeechSynthesizer func(SpeechClientOptions) (SpeechSynthesizer, error)

type WriteArtifact func(string, []byte) error

type VoiceInstructionOptions struct {
	ArtifactDir     string
	InstructionPath string
	ErrorLabel      string
	TTSErrorLabel   string
	Provider        string
	BaseURL         string
	APIKey          []byte
	Model           string
	Voice           string
	Instructions    string
	Speed           *float64
	Timeout         time.Duration
	NewSpeech       NewSpeechSynthesizer
	WriteArtifact   WriteArtifact
}

type VoiceInstructionMetadata struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Voice      string `json:"voice"`
	Format     string `json:"format"`
	TextSHA256 string `json:"text_sha256"`
	TextChars  int    `json:"text_chars"`
	Cached     bool   `json:"cached"`
	OutputPath string `json:"output_path"`
}

func SynthesizeVoiceInstruction(ctx context.Context, instruction string, options VoiceInstructionOptions) (string, []string, error) {
	instructionPath := options.InstructionPath
	if instructionPath == "" {
		instructionPath = filepath.Join(options.ArtifactDir, VoiceInstructionTextFile)
		if err := options.WriteArtifact(instructionPath, []byte(instruction)); err != nil {
			return "", nil, fmt.Errorf("write %s voice instruction: %w", options.ErrorLabel, err)
		}
	}
	synthesizer, err := options.NewSpeech(SpeechClientOptions{BaseURL: options.BaseURL, APIKey: options.APIKey, Timeout: options.Timeout})
	if err != nil {
		return "", []string{instructionPath}, fmt.Errorf("construct %s TTS client: %w", options.ttsErrorLabel(), err)
	}
	defer synthesizer.Close()
	result, err := synthesizer.Synthesize(ctx, SpeechRequest{
		Text: instruction, Model: options.Model, Voice: options.Voice,
		Format: DefaultSpeechFormat, Instructions: options.Instructions, Speed: options.Speed,
	})
	if err != nil {
		return "", []string{instructionPath}, fmt.Errorf("synthesize %s voice instruction: %w", options.ErrorLabel, err)
	}
	audioPath := filepath.Join(options.ArtifactDir, VoiceInstructionWAVFile)
	if err := options.WriteArtifact(audioPath, result.Audio); err != nil {
		clear(result.Audio)
		return "", []string{instructionPath}, fmt.Errorf("write %s voice audio: %w", options.ErrorLabel, err)
	}
	clear(result.Audio)
	metaPath := filepath.Join(options.ArtifactDir, VoiceInstructionMetaFile)
	metadata := VoiceInstructionMetadata{
		Provider: options.Provider, Model: result.Model, Voice: result.Voice, Format: result.Format,
		TextSHA256: result.TextSHA256, TextChars: len(instruction), Cached: false, OutputPath: audioPath,
	}
	content, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", []string{instructionPath, audioPath}, fmt.Errorf("encode %s TTS metadata: %w", options.ttsErrorLabel(), err)
	}
	content = append(content, '\n')
	if err := options.WriteArtifact(metaPath, content); err != nil {
		return "", []string{instructionPath, audioPath}, fmt.Errorf("write %s TTS metadata: %w", options.ttsErrorLabel(), err)
	}
	return audioPath, []string{instructionPath, audioPath, metaPath}, nil
}

func (options VoiceInstructionOptions) ttsErrorLabel() string {
	if options.TTSErrorLabel != "" {
		return options.TTSErrorLabel
	}
	return options.ErrorLabel + " voice"
}
