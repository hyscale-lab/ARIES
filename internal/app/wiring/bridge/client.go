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
	case "codex-ssh":
		if helperPath == "" {
			return client, errors.New("Codex bridge requires local client helper")
		}
		// Codex's environments.toml spawns this program; the harness stages it
		// into its private runtime at exactly this path.
		client.Command = "/run/aries/codex/aries-codex-ssh"
		client.SourcePath = helperPath
		return client, nil
	default:
		return client, errors.New("unsupported SSH harness bridge")
	}
}
