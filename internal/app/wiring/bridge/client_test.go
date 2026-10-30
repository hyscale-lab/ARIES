package bridge

import (
	"testing"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
)

func TestClientConfigSelectsTheHelperOfTheBridgeType(t *testing.T) {
	const helperDir = "/opt/build/bin"
	for bridgeType, want := range map[string]managed.ClientConfig{
		"hermes-ssh":   {},
		"openclaw-ssh": {Command: "/opt/aries/bin/aries-ssh-client", SourcePath: "/opt/build/bin/aries-ssh-client"},
		"hermes-grpc":  {},
	} {
		got, err := ClientConfig(bridgeType, helperDir)
		if err != nil || got != want {
			t.Fatalf("ClientConfig(%q) = %#v, %v; want %#v", bridgeType, got, err, want)
		}
	}
	for _, bridgeType := range []string{"openclaw-ssh"} {
		if _, err := ClientConfig(bridgeType, ""); err == nil {
			t.Fatalf("ClientConfig(%q) accepted a missing helper directory", bridgeType)
		}
	}
	if _, err := ClientConfig("other", helperDir); err == nil {
		t.Fatal("unknown bridge type accepted")
	}
}
