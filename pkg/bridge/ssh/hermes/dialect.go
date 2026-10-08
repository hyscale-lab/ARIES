// Package hermes adapts Hermes's native OpenSSH command dialect.
package hermes

import (
	"errors"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
)

// Dialect preserves Hermes's probes, Bash grammar, and private-sync policy.
// It owns no transport or execution lifecycle.
type Dialect struct{}

var _ ssh.Dialect = Dialect{}

func (Dialect) Policy() ssh.Policy {
	return ssh.Policy{
		UnsupportedRequests:   ssh.RejectAndContinue,
		InvalidOperationClass: kindUnknown,
		RefusedExitCode:       -1,
	}
}

func (Dialect) Prepare(encoded, workdir string) (ssh.Prepared, *ssh.Refusal) {
	remote, err := decodeRemoteCommand(encoded)
	if err != nil {
		if errors.Is(err, errSyncDenied) {
			return ssh.Prepared{}, &ssh.Refusal{OperationClass: kindSync, Status: "denied", Message: errSyncDenied.Error()}
		}
		return ssh.Prepared{}, rejected(kindUnknown)
	}
	prepared, err := prepareRemoteCommand(remote, workdir)
	if err != nil {
		return ssh.Prepared{}, rejected(remote.kind)
	}
	return ssh.Prepared{
		Command: prepared.command, Action: ssh.Execute,
		HashInput: prepared.encoded, Display: prepared.encoded,
		OperationClass: prepared.kind, RefusalClass: prepared.kind,
	}, nil
}

func rejected(kind string) *ssh.Refusal {
	return &ssh.Refusal{OperationClass: kind, Status: "rejected", Message: "invalid remote command"}
}
