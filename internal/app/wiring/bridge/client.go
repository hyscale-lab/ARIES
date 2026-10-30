package bridge

import (
	"errors"
	"path/filepath"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
)

// ClientConfig selects the harness-side client setup. Native servers only own
// command and workspace semantics; the controller stages these explicit inputs.
// A staged helper is taken from helperDir under its container command's name.
func ClientConfig(bridgeType, helperDir string) (managed.ClientConfig, error) {
	client := managed.ClientConfig{}
	switch bridgeType {
	case "hermes-ssh", "hermes-grpc":
		return client, nil
	case "openclaw-ssh":
		client.Command = "/opt/aries/bin/aries-ssh-client"
	default:
		return client, errors.New("unsupported harness bridge")
	}
	if helperDir == "" {
		return managed.ClientConfig{}, errors.New("bridge requires local client helper")
	}
	client.SourcePath = filepath.Join(helperDir, filepath.Base(client.Command))
	return client, nil
}
