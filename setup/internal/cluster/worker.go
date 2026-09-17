package cluster

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/node"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

var (
	joinEndpoint = regexp.MustCompile(`^(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+):[0-9]{1,5}$`)
	joinToken    = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
	joinCAHash   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// JoinCommand is a parsed `kubeadm join`. It is parsed and re-rendered rather
// than executed as given: the worker step runs it through a shell, so only
// fields that match kubeadm's own formats ever reach the command line.
type JoinCommand struct {
	Endpoint string
	Token    string
	CAHash   string
}

// ParseJoinCommand accepts the line printed by
// `kubeadm token create --print-join-command`, with or without a leading sudo
// and a trailing --cri-socket.
func ParseJoinCommand(line string) (JoinCommand, error) {
	fields := strings.Fields(line)
	if len(fields) > 0 && fields[0] == "sudo" {
		fields = fields[1:]
	}
	if len(fields) < 3 || fields[0] != "kubeadm" || fields[1] != "join" {
		return JoinCommand{}, fmt.Errorf("not a `kubeadm join` command: %q", line)
	}
	join := JoinCommand{Endpoint: fields[2]}
	for i := 3; i < len(fields); i += 2 {
		if i+1 >= len(fields) {
			return JoinCommand{}, fmt.Errorf("join command flag %s has no value", fields[i])
		}
		switch value := fields[i+1]; fields[i] {
		case "--token":
			join.Token = value
		case "--discovery-token-ca-cert-hash":
			join.CAHash = value
		case "--cri-socket":
			// Replaced by the node's configured socket.
		default:
			return JoinCommand{}, fmt.Errorf("unsupported join command flag %s", fields[i])
		}
	}
	return join, join.Validate()
}

// Validate checks each field against kubeadm's formats.
func (j JoinCommand) Validate() error {
	switch {
	case !joinEndpoint.MatchString(j.Endpoint):
		return fmt.Errorf("join endpoint %q must be host:port", j.Endpoint)
	case !joinToken.MatchString(j.Token):
		return fmt.Errorf("join token must look like abcdef.0123456789abcdef")
	case !joinCAHash.MatchString(j.CAHash):
		return fmt.Errorf("discovery hash must look like sha256:<64 hex>")
	}
	return nil
}

// String renders the command without a CRI socket, as kubeadm prints it.
func (j JoinCommand) String() string {
	return fmt.Sprintf("kubeadm join %s --token %s --discovery-token-ca-cert-hash %s", j.Endpoint, j.Token, j.CAHash)
}

// SetupWorker joins this node to the control plane. It expects setup_node to
// have run, and refuses a node that already belongs to a cluster.
func SetupWorker(join JoinCommand, kube configs.Kube, system configs.System) error {
	if err := utils.RequireRoot(); err != nil {
		return err
	}
	if err := join.Validate(); err != nil {
		return err
	}
	if _, err := utils.ExecShellCmd("command -v kubeadm"); err != nil {
		return fmt.Errorf("kubeadm is not installed; run setup_node first")
	}
	if _, err := os.Stat("/etc/kubernetes/kubelet.conf"); err == nil {
		return fmt.Errorf("this node already belongs to a cluster; run reset_node --yes before joining again")
	}
	if err := node.OpenFirewall(node.RoleWorker, kube); err != nil {
		return err
	}

	utils.WaitPrintf("Joining the cluster at %s", join.Endpoint)
	if _, err := utils.ExecShellCmd("kubeadm join %s --token %s --discovery-token-ca-cert-hash %s --cri-socket %s",
		utils.Quote(join.Endpoint), utils.Quote(join.Token), utils.Quote(join.CAHash), utils.Quote(system.CRISocket)); err != nil {
		return err
	}
	utils.SuccessPrintf("worker joined; it reports Ready once the CNI DaemonSet schedules on it")
	return nil
}
