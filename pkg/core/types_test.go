package core

import "testing"

func TestModelConfigUsesDockerHost(t *testing.T) {
	for baseURL, want := range map[string]bool{
		"http://host.docker.internal:8080/v1": true,
		"http://host.docker.internal/v1":      true,
		"http://echo-model:8080/v1":           false,
		"http://host.docker.internal.evil/v1": false,
		"https://api.deepseek.com":            false,
		"":                                    false,
	} {
		if got := (ModelConfig{BaseURL: baseURL}).UsesDockerHost(); got != want {
			t.Errorf("UsesDockerHost(%q) = %v, want %v", baseURL, got, want)
		}
	}
}
