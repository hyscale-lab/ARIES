package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSpeechClientPostsOpenAICompatibleSpeechRequest(t *testing.T) {
	var authorization string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/audio/speech" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		authorization = request.Header.Get("Authorization")
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "tts-model" || payload["input"] != "repair git" || payload["voice"] != "alloy" || payload["response_format"] != "wav" || payload["instructions"] != "be clear" || payload["speed"] != 1.25 {
			t.Fatalf("payload = %#v", payload)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"audio/wav"}},
			Body:       io.NopCloser(bytes.NewReader([]byte("RIFF....WAVE"))),
		}, nil
	})

	key := []byte("speech-secret")
	speed := 1.25
	client, err := NewSpeechClient(SpeechClientOptions{BaseURL: "http://tts.invalid/v1", APIKey: key, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	result, err := client.Synthesize(context.Background(), SpeechRequest{
		Text: " repair git ", Model: "tts-model", Voice: "alloy",
		Format: "wav", Instructions: "be clear", Speed: &speed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer speech-secret" {
		t.Fatalf("authorization = %q", authorization)
	}
	if !bytes.Equal(result.Audio, []byte("RIFF....WAVE")) || result.Model != "tts-model" || result.Voice != "alloy" || result.Format != "wav" || len(result.TextSHA256) != 64 {
		t.Fatalf("result = %#v", result)
	}
	client.Close()
	if _, err := client.Synthesize(context.Background(), SpeechRequest{Text: "again"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed synth error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestSpeechClientRejectsUnsafeInputs(t *testing.T) {
	if _, err := NewSpeechClient(SpeechClientOptions{BaseURL: "https://user:pass@example.test/v1", APIKey: []byte("key")}); err == nil {
		t.Fatal("accepted credentialed base URL")
	}
	client, err := NewSpeechClient(SpeechClientOptions{BaseURL: "https://example.test/v1", APIKey: []byte("key")})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Synthesize(context.Background(), SpeechRequest{Text: "hello", Format: "mp3"}); err == nil || !strings.Contains(err.Error(), "wav") {
		t.Fatalf("format error = %v", err)
	}
}
