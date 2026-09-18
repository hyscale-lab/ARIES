package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSynthesizeVoiceInstructionWritesArtifacts(t *testing.T) {
	speed := 1.1
	var speechOptions SpeechClientOptions
	var speechRequest SpeechRequest
	synthesizer := &stubInstructionSynthesizer{result: SpeechResult{
		Audio: []byte("RIFF....WAVE"), Model: "tts-model", Voice: "alloy", Format: "wav", TextSHA256: "abc123",
	}}
	writes := map[string][]byte{}
	audioPath, paths, err := SynthesizeVoiceInstruction(context.Background(), " repair git ", VoiceInstructionOptions{
		ArtifactDir: filepath.Join(t.TempDir(), "harness"),
		ErrorLabel:  "test",
		Provider:    "openai",
		BaseURL:     "http://tts.invalid/v1",
		APIKey:      []byte("speech-secret"),
		Model:       "tts-model",
		Voice:       "alloy",
		Speed:       &speed,
		Timeout:     time.Second,
		NewSpeech: func(options SpeechClientOptions) (SpeechSynthesizer, error) {
			speechOptions = options
			return synthesizer, nil
		},
		WriteArtifact: func(path string, content []byte) error {
			writes[filepath.Base(path)] = bytes.Clone(content)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(audioPath) != VoiceInstructionWAVFile || len(paths) != 3 {
		t.Fatalf("audioPath=%q paths=%#v", audioPath, paths)
	}
	if string(writes[VoiceInstructionTextFile]) != " repair git " || string(writes[VoiceInstructionWAVFile]) != "RIFF....WAVE" {
		t.Fatalf("writes = %#v", writes)
	}
	if speechOptions.BaseURL != "http://tts.invalid/v1" || !bytes.Equal(speechOptions.APIKey, []byte("speech-secret")) || speechOptions.Timeout != time.Second {
		t.Fatalf("speech options = %#v", speechOptions)
	}
	if speechRequest = synthesizer.request; speechRequest.Text != " repair git " || speechRequest.Model != "tts-model" || speechRequest.Voice != "alloy" || speechRequest.Format != "wav" || speechRequest.Speed != &speed {
		t.Fatalf("speech request = %#v", speechRequest)
	}
	if !synthesizer.closed {
		t.Fatal("synthesizer was not closed")
	}
	if !bytes.Equal(synthesizer.result.Audio, make([]byte, len("RIFF....WAVE"))) {
		t.Fatalf("audio buffer was not cleared: %q", synthesizer.result.Audio)
	}
	var metadata VoiceInstructionMetadata
	if err := json.Unmarshal(writes[VoiceInstructionMetaFile], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Provider != "openai" || metadata.Model != "tts-model" || metadata.Voice != "alloy" || metadata.Format != "wav" || metadata.TextSHA256 != "abc123" || metadata.TextChars != len(" repair git ") || metadata.Cached {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestSynthesizeVoiceInstructionUsesSuppliedInstructionPath(t *testing.T) {
	instructionPath := filepath.Join(t.TempDir(), VoiceInstructionTextFile)
	synthesizer := &stubInstructionSynthesizer{result: SpeechResult{
		Audio: []byte("RIFF....WAVE"), Model: "tts-model", Voice: "alloy", Format: "wav", TextSHA256: "abc123",
	}}
	writes := map[string]int{}
	audioPath, paths, err := SynthesizeVoiceInstruction(context.Background(), "repair git", VoiceInstructionOptions{
		ArtifactDir:     filepath.Join(t.TempDir(), "harness"),
		InstructionPath: instructionPath,
		ErrorLabel:      "test",
		Provider:        "openai",
		Model:           "tts-model",
		Voice:           "alloy",
		NewSpeech: func(SpeechClientOptions) (SpeechSynthesizer, error) {
			return synthesizer, nil
		},
		WriteArtifact: func(path string, _ []byte) error {
			writes[path]++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || paths[0] != instructionPath || audioPath != paths[1] {
		t.Fatalf("audioPath=%q paths=%#v", audioPath, paths)
	}
	if writes[instructionPath] != 0 {
		t.Fatalf("supplied instruction path was written %d time(s)", writes[instructionPath])
	}
	if writes[filepath.Join(filepath.Dir(audioPath), VoiceInstructionWAVFile)] != 1 || writes[filepath.Join(filepath.Dir(audioPath), VoiceInstructionMetaFile)] != 1 {
		t.Fatalf("writes = %#v", writes)
	}
}

func TestSynthesizeVoiceInstructionReturnsPartialArtifactsOnError(t *testing.T) {
	_, paths, err := SynthesizeVoiceInstruction(context.Background(), "repair git", VoiceInstructionOptions{
		ArtifactDir: t.TempDir(),
		ErrorLabel:  "test",
		NewSpeech: func(SpeechClientOptions) (SpeechSynthesizer, error) {
			return nil, errors.New("boom")
		},
		WriteArtifact: func(string, []byte) error {
			return nil
		},
	})
	if err == nil || len(paths) != 1 {
		t.Fatalf("err=%v paths=%#v", err, paths)
	}
}

type stubInstructionSynthesizer struct {
	request SpeechRequest
	result  SpeechResult
	closed  bool
}

func (stub *stubInstructionSynthesizer) Synthesize(_ context.Context, request SpeechRequest) (SpeechResult, error) {
	stub.request = request
	return stub.result, nil
}

func (stub *stubInstructionSynthesizer) Close() {
	stub.closed = true
}
