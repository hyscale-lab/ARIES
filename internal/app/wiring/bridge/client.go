package bridge

import (
	"errors"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
)

// SSHClientConfig selects the harness-side SSH setup. Native dialects only own
// command and workspace semantics; the controller stages these explicit inputs.
func SSHClientConfig(bridgeType, helperPath string) (managed.ClientConfig, error) {
	client := managed.ClientConfig{}
	switch bridgeType {
	case "hermes-ssh":
		return client, nil
	case "openclaw-ssh":
		if helperPath == "" {
			return client, errors.New("OpenClaw bridge requires local client helper")
		}
		client.Command = "/opt/aries/bin/aries-ssh-client"
		client.SourcePath = helperPath
		return client, nil
	default:
		return client, errors.New("unsupported SSH harness bridge")
	}
}
