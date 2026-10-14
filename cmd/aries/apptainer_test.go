package main

import (
	"slices"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestApptainerImagesLeaveOutOnlyHermesBase(t *testing.T) {
	images := []string{"example.test/task:1", "docker.io/nousresearch/hermes-agent:v1"}
	cfg := config.Config{Harness: config.HarnessConfig{Type: "hermes"}}
	cfg.Versions.Hermes.Image = "docker.io/nousresearch/hermes-agent:v1"
	if got := apptainerImages(cfg, images); !slices.Equal(got, []string{"example.test/task:1"}) {
		t.Fatalf("Hermes images = %q", got)
	}
	cfg.Harness.Type = "openclaw"
	if got := apptainerImages(cfg, images); !slices.Equal(got, images) {
		t.Fatalf("OpenClaw images = %q", got)
	}
}
