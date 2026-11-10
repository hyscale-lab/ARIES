package config

import "testing"

func TestStepObservationProfileConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, kind, mode, dir, url string
		ok                         bool
	}{
		{"valid", "openclaw", "agent", "/home/operator/plugin", "http://172.17.0.1:9100", true},
		{"default mode", "openclaw", "", "/home/operator/plugin", "https://observer.example", true},
		{"hermes", "hermes", "agent", "/plugin", "http://localhost", false},
		{"realtime", "openclaw", "realtime", "/plugin", "http://localhost", false},
		{"relative plugin", "openclaw", "agent", "plugin", "http://localhost", false},
		{"credential", "openclaw", "agent", "/plugin", "http://key@localhost", false},
		{"query", "openclaw", "agent", "/plugin", "http://localhost/?key=value", false},
		{"protocol", "openclaw", "agent", "/plugin", "file:///tmp/x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := HarnessConfig{Type: tc.kind, Mode: tc.mode, StepObservation: &HarnessStepObservationConfig{PluginDir: tc.dir, ObserverURL: tc.url}}
			err := h.validate()
			if (err == nil) != tc.ok {
				t.Fatalf("validate=%v wantOK=%v", err, tc.ok)
			}
		})
	}
}
