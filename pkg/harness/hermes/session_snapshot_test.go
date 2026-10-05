package hermes

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/harness"

	audioinput "github.com/hyscale-lab/aries/pkg/audio"
)

func TestConcurrentRunStopKeepsPrivateVoiceSnapshot(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("unused"))
	secret := "voice-private-key"
	manager.options.Common.Mode = ModeVoiceTranscribe
	directory := t.TempDir()
	owner := &session{Occurrence: &harness.Occurrence{ID: "owned-runtime", ArtifactDir: directory, Credentials: harness.NewCredentials("Hermes"), Artifacts: &harness.Artifacts{Directory: directory}}}
	owner.Credentials.Set("voice", []byte(secret))
	if err := manager.runtime.Own(owner.Occurrence); err != nil {
		t.Fatal(err)
	}
	manager.runtime.Ready()
	manager.active = owner
	entered, release := make(chan struct{}), make(chan struct{})
	var runSecret []byte
	manager.newSpeech = func(options audioinput.SpeechClientOptions) (speechSynthesizer, error) {
		runSecret = options.APIKey
		close(entered)
		<-release
		if string(options.APIKey) != secret {
			return nil, errors.New("run credential was cleared by Stop")
		}
		return nil, errors.New("provider echoed " + secret)
	}
	result := make(chan error, 1)
	go func() { _, err := manager.Run(context.Background(), "task"); result <- err }()
	<-entered
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if owner.Credentials.Get("voice") != nil || owner.ID != "" {
		t.Fatal("Stop retained owner resources")
	}
	close(release)
	err := <-result
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("run lost redaction after concurrent stop: %v", err)
	}
	if !bytes.Equal(runSecret, make([]byte, len(runSecret))) {
		t.Fatal("Run retained copied secret after returning")
	}
}
