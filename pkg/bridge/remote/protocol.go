// Package remote runs a tool bridge in its own pod, apart from the runner.
//
// The runner and the bridge meet only twice per task: once to grant a
// sandbox and once to revoke it. Tool calls go harness pod → bridge pod →
// sandbox pod and never pass through the runner, so the control path can be
// slow and simple. It is `kubectl exec` into the bridge pod, running
// `aries-bridge ctl`, which relays one JSON request to the daemon over a Unix
// socket. Nothing is exposed on the network, and who may drive the bridge is
// decided by Kubernetes RBAC (pods/exec on the bridge pod).
//
//	runner (Client) ──kubectl exec── aries-bridge ctl ──unix socket── Daemon
//	                                                                   │ per grant
//	harness pod ──SSH──────────────────────────────────────────► hermes/openclaw
//	                                                             SSH bridge
//	                                                                   │ kubectl exec
//	                                                                   ▼
//	                                                              sandbox pod
//
// The daemon holds grants in memory only. That is what makes revocation
// provable from outside: if the process that served a grant is gone, every
// session it served is gone with it.
package remote

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
)

// Operations the daemon accepts.
const (
	OpGrant   = "grant"
	OpRevoke  = "revoke"
	OpCollect = "collect"
	OpRelease = "release"
	OpStatus  = "status"
)

// Grant states a response can report.
const (
	StateActive  = "active"
	StateRevoked = "revoked"
	// StateAbsent means the daemon holds no grant under that ID: it never
	// existed, it was released, or the daemon restarted. In every case no
	// session for it is being served.
	StateAbsent = "absent"
)

// The bridge's fixed identity. The chart, the compose file and the runner
// must agree on these; they are constants rather than configuration so they
// cannot drift through a profile.
const (
	PodSelector   = "app.kubernetes.io/name=aries-bridge"
	ContainerName = "bridge"
	// ContainerLabel marks the Docker bridge container (docker/docker-compose.yml).
	ContainerLabel = "aries.component=tool-bridge"
	Executable     = "/usr/local/bin/aries-bridge"
	DefaultSocket  = "/run/aries-bridge/control.sock"
)

// maxMessageBytes bounds one request or response on the socket.
const maxMessageBytes = 1 << 20

// Request is one control message. GrantID is chosen by the runner, not the
// daemon, so a runner whose grant call failed in transit can still revoke it
// by ID.
type Request struct {
	Op      string        `json:"op"`
	GrantID string        `json:"grant_id,omitempty"`
	Grant   *GrantRequest `json:"grant,omitempty"`
}

// GrantRequest names the sandbox to serve and the key to accept. Every field
// comes from the runner, which owns the sandbox; the daemon re-checks the
// sandbox against them before serving it.
type GrantRequest struct {
	BridgeType string     `json:"bridge_type"`
	Sandbox    SandboxRef `json:"sandbox"`
	// ListenHost, when set, is the address the grant listens and is
	// advertised on: the bridge's own address on the task's network, which
	// the runner attached it to. Empty uses the bridge's default address.
	ListenHost string `json:"listen_host,omitempty"`
	// AuthorizedKey is the client public key in authorized_keys format. The
	// private half stays with the runner.
	AuthorizedKey string `json:"authorized_key"`
	RetainRawLog  bool   `json:"retain_raw_log,omitempty"`
}

// Response is the daemon's answer. Instance is random per daemon process and
// is reported on every response, so a restart is visible in the logs.
type Response struct {
	Instance string         `json:"instance"`
	State    string         `json:"state,omitempty"`
	Error    string         `json:"error,omitempty"`
	Grant    *GrantResponse `json:"grant,omitempty"`
	// Files are the grant's log files inside the pod, for collect.
	Files []string `json:"files,omitempty"`
	// Grants counts live grants, for status.
	Grants int `json:"grants,omitempty"`
}

// GrantResponse is what the harness needs to reach the bridge.
type GrantResponse struct {
	Address string `json:"address"`
	// HostKey is the SSH host public key in authorized_keys format.
	HostKey string `json:"host_key"`
	Network string `json:"network"`
	// LogFiles are the base names of the grant's log files, which collect
	// returns and the runner writes under the same names.
	LogFiles []string `json:"log_files"`
}

var grantIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func validateGrantID(id string) error {
	if !grantIDPattern.MatchString(id) {
		return fmt.Errorf("grant ID %q is not 32 lowercase hex characters", id)
	}
	return nil
}

// logFileNames is every log a bridge can write. Collect refuses anything
// else, so a daemon cannot make the runner write an arbitrary file.
var logFileNames = map[string]bool{"tool-calls.jsonl": true, "ssh_raw.log": true}

func validateLogFile(name string) error {
	if !logFileNames[name] {
		return fmt.Errorf("unexpected bridge log file %q", name)
	}
	return nil
}

func validateGrantRequest(request *GrantRequest) error {
	if request == nil {
		return errors.New("grant request is missing")
	}
	switch request.BridgeType {
	case "hermes-ssh", "openclaw-ssh":
	default:
		return fmt.Errorf("bridge type %q cannot run as a separate bridge", request.BridgeType)
	}
	sandbox := request.Sandbox
	required := map[string]string{"run_id": sandbox.RunID, "task_id": sandbox.TaskID, "authorized_key": request.AuthorizedKey}
	switch sandbox.Backend {
	case BackendKubernetes:
		required["namespace"], required["pod_name"], required["sandbox_id"] = sandbox.Namespace, sandbox.PodName, sandbox.SandboxID
	case BackendDocker:
		required["container_id"], required["network"] = sandbox.ContainerID, sandbox.Network
	default:
		return fmt.Errorf("unknown sandbox backend %q", sandbox.Backend)
	}
	for name, value := range required {
		if value == "" {
			return fmt.Errorf("grant request needs %s", name)
		}
	}
	if request.ListenHost != "" {
		if address, err := netip.ParseAddr(request.ListenHost); err != nil || !address.Is4() {
			return fmt.Errorf("listen_host %q is not an IPv4 address", request.ListenHost)
		}
	}
	return nil
}
