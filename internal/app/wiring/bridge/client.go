package bridge

import (
	"errors"
	"path/filepath"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
)

// SSHClientConfig selects the harness-side SSH setup. Native dialects only own
// command and workspace semantics; the controller stages these explicit inputs.
// binDir holds the static helpers built beside the aries executable.
func SSHClientConfig(bridgeType, binDir string) (managed.ClientConfig, error) {
	client := managed.ClientConfig{}
	switch bridgeType {
	case "hermes-ssh":
		return client, nil
	case "openclaw-ssh":
		if binDir == "" {
			return client, errors.New("OpenClaw bridge requires local client helper")
		}
		client.Command = "/opt/aries/bin/aries-ssh-client"
		client.SourcePath = filepath.Join(binDir, "aries-ssh-client")
		return client, nil
	case "codex-ssh":
		if binDir == "" {
			return client, errors.New("Codex bridge requires local client helper")
		}
		// Codex's environments.toml spawns this program; the harness stages it
		// into its private runtime at exactly this path.
		client.Command = "/run/aries/codex/aries-codex-ssh"
		client.SourcePath = filepath.Join(binDir, "aries-codex-ssh")
		return client, nil
	default:
		return client, errors.New("unsupported SSH harness bridge")
	}
}
