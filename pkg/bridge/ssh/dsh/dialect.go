// Package dsh adapts DSH's native SSH helper transport to one borrowed sandbox.
package dsh

import (
	"path"
	"strings"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/core"
)

const RuntimeRoot = "/tmp/aries-dsh-runtime"
const HelperPath = RuntimeRoot + "/node_modules/@deepseek-ai/dsh-ssh/lib/helper.js"
const HelperCommand = "'" + RuntimeRoot + "/node' '--disable-sigusr1' '" + HelperPath + "'"

type Dialect struct{}

func (Dialect) Policy() ssh.Policy {
	return ssh.Policy{UnsupportedRequests: ssh.RejectAndContinue, InvalidOperationClass: "dsh-protocol", RefusedExitCode: 255, StreamingInput: true}
}

func (Dialect) Prepare(encoded, workdir string) (ssh.Prepared, *ssh.Refusal) {
	if encoded != HelperCommand {
		return ssh.Prepared{}, &ssh.Refusal{OperationClass: "dsh-protocol", Status: "rejected", Message: "expected the pinned DSH helper invocation"}
	}
	return ssh.Prepared{Command: core.Command{Path: RuntimeRoot + "/node", Args: []string{"--disable-sigusr1", HelperPath}, Dir: workdir, StreamProtocol: true}, HashInput: encoded, Display: encoded, OperationClass: "dsh-helper", RefusalClass: "dsh-helper"}, nil
}

// PrepareStreamLocal keeps native TLS stream authentication inside DSH. The
// Unix socket is reached inside the same sandbox, never on the bridge host.
func (Dialect) PrepareStreamLocal(socket, workdir string) (ssh.Prepared, *ssh.Refusal) {
	rest, ok := strings.CutPrefix(socket, "/tmp/dsh-ssh-")
	root, suffix, found := strings.Cut(rest, "/")
	if !ok || !found || root == "" || suffix == "" || path.Clean(socket) != socket || strings.ContainsAny(socket, "\x00\r\n:") {
		return ssh.Prepared{}, &ssh.Refusal{OperationClass: "dsh-stream", Status: "rejected", Message: "invalid DSH stream socket"}
	}
	return ssh.Prepared{Command: core.Command{Path: RuntimeRoot + "/node", Args: []string{RuntimeRoot + "/stream.mjs", socket}, Dir: workdir, StreamProtocol: true}, HashInput: socket, Display: socket, OperationClass: "dsh-stream", RefusalClass: "dsh-stream"}, nil
}
