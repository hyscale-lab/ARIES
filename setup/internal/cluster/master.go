// Package cluster turns prepared nodes into a cluster: the control-plane and
// worker steps that run on a node, and create_cluster, which drives them over
// SSH from the operator's machine.
package cluster

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/node"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

const adminConf = "/etc/kubernetes/admin.conf"

// kubectlAdmin prefixes kubectl with the admin kubeconfig, which exists before
// any user's kubeconfig is installed.
const kubectlAdmin = "KUBECONFIG=" + adminConf + " kubectl"

// SetupMasterNode initialises the control plane with kubeadm, installs the CNI
// and records a join command for workers. It expects setup_node to have run.
func SetupMasterNode(kube configs.Kube, system configs.System) error {
	if err := utils.RequireRoot(); err != nil {
		return err
	}
	if _, err := utils.ExecShellCmd("command -v kubeadm"); err != nil {
		return fmt.Errorf("kubeadm is not installed; run setup_node first")
	}
	if err := node.OpenFirewall(node.RoleControlPlane, kube); err != nil {
		return err
	}

	if _, err := os.Stat(adminConf); err == nil {
		utils.InfoPrintf("control plane already initialised; skipping kubeadm init")
	} else if err := kubeadmInit(kube, system); err != nil {
		return err
	}

	utils.WaitPrintf("Installing kubeconfig for root and the sudo user")
	if err := installKubeconfig(); err != nil {
		return err
	}
	if err := waitForAPI(); err != nil {
		return err
	}
	if err := installCNI(kube, system); err != nil {
		return err
	}

	if kube.SingleNode {
		utils.WaitPrintf("Removing the control-plane taint (single_node)")
		_, _ = utils.ExecShellCmd(kubectlAdmin + " taint nodes --all node-role.kubernetes.io/control-plane-")
	}

	utils.WaitPrintf("Waiting for this node to become Ready")
	if _, err := utils.ExecShellCmd(kubectlAdmin + " wait --for=condition=Ready node --all --timeout=300s"); err != nil {
		utils.WarnPrintf("node not Ready yet; check `kubectl get pods -A` for CNI pods")
	}

	join, err := writeJoinFile(system)
	if err != nil {
		return err
	}
	nodes, _ := utils.ExecShellCmd(kubectlAdmin + " get nodes -o wide")
	utils.InfoPrintf("\n%s", nodes)
	utils.SuccessPrintf("control plane is up")
	utils.InfoPrintf("join a worker by running on it:")
	utils.InfoPrintf("  sudo aries-setup setup_worker --join %s", utils.Quote(join.String()))
	utils.InfoPrintf("the join command is also saved at %s (root only, valid 24h)", system.JoinFile)
	return nil
}

func kubeadmInit(kube configs.Kube, system configs.System) error {
	address := kube.AdvertiseAddress
	if address == "" {
		// The source address of the route to the default gateway is the
		// interface workers reach this node on.
		detected, err := utils.ExecShellCmd(`ip -4 route get 1.1.1.1 | awk '{for (i=1;i<NF;i++) if ($i=="src") {print $(i+1); exit}}'`)
		if err != nil || detected == "" {
			return fmt.Errorf("cannot determine the advertise address; set advertise_address in kube.json")
		}
		address = detected
	}

	utils.WaitPrintf("Pre-pulling control-plane images")
	if _, err := utils.ExecShellCmd("kubeadm config images pull --cri-socket %s", utils.Quote(system.CRISocket)); err != nil {
		return err
	}

	args := []string{
		"--cri-socket", system.CRISocket,
		"--pod-network-cidr", kube.PodCIDR,
		"--apiserver-advertise-address", address,
	}
	if kube.K8sVersion != "" {
		args = append(args, "--kubernetes-version", "v"+kube.K8sVersion)
	}
	if kube.ControlPlaneEndpoint != "" {
		args = append(args, "--control-plane-endpoint", kube.ControlPlaneEndpoint, "--upload-certs")
	}
	utils.WaitPrintf("Running kubeadm init (advertise %s, pod CIDR %s); this takes a few minutes", address, kube.PodCIDR)
	return utils.ExecShellCmdStreaming("kubeadm init %s", utils.QuoteAll(args...))
}

// installKubeconfig makes the cluster usable as root and as the user who
// invoked sudo.
func installKubeconfig() error {
	if _, err := utils.ExecShellCmd("install -m 0700 -d /root/.kube && install -m 0600 %s /root/.kube/config", adminConf); err != nil {
		return err
	}
	home := node.SudoUserHome()
	if home == "" {
		return nil
	}
	user := os.Getenv("SUDO_USER")
	// Not every site gives a user a like-named group (CloudLab puts users in a
	// shared project group), so ask for the primary group rather than assume.
	group, err := utils.ExecShellCmd("id -gn %s", utils.Quote(user))
	if err != nil || group == "" {
		group = user
	}
	kubeDir := home + "/.kube"
	if _, err := utils.ExecShellCmd("install -m 0700 -o %s -g %s -d %s && install -m 0600 -o %s -g %s %s %s",
		utils.Quote(user), utils.Quote(group), utils.Quote(kubeDir),
		utils.Quote(user), utils.Quote(group), adminConf, utils.Quote(kubeDir+"/config")); err != nil {
		return err
	}
	utils.InfoPrintf("kubeconfig installed for %s at %s/config", user, kubeDir)
	return nil
}

func waitForAPI() error {
	utils.WaitPrintf("Waiting for the API server to serve requests")
	for range 60 {
		if _, err := utils.ExecShellCmd(kubectlAdmin + " get --raw=/readyz"); err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("API server did not become ready; inspect `journalctl -u kubelet` and `crictl ps -a`")
}

func installCNI(kube configs.Kube, system configs.System) error {
	switch kube.CNI {
	case configs.CNINone:
		utils.WarnPrintf("no CNI installed; nodes stay NotReady until you apply one")
		return nil
	case configs.CNIFlannel:
		utils.WaitPrintf("Installing Flannel")
		if kube.PodCIDR != configs.FlannelPodCIDR {
			utils.WarnPrintf("Flannel's manifest hard-codes %s; pod CIDR %s needs a patched manifest", configs.FlannelPodCIDR, kube.PodCIDR)
		}
		_, err := utils.ExecShellCmd(kubectlAdmin+" apply -f %s", utils.Quote(system.FlannelManifest))
		return err
	}

	utils.WaitPrintf("Installing Calico %s (Tigera operator)", kube.CalicoVersion)
	base := fmt.Sprintf("%s/%s/manifests", system.CalicoManifestBase, kube.CalicoVersion)
	// Server-side apply: the operator bundle carries CRDs larger than the
	// client-side last-applied annotation limit, and it must stay re-runnable.
	for _, manifest := range []string{"v1_crd_projectcalico_org.yaml", "tigera-operator.yaml"} {
		if _, err := utils.ExecShellCmd(kubectlAdmin+" apply --server-side --force-conflicts -f %s", utils.Quote(base+"/"+manifest)); err != nil {
			return err
		}
	}
	if _, err := utils.ExecShellCmd(kubectlAdmin + " wait --for=condition=Available --timeout=180s -n tigera-operator deployment/tigera-operator"); err != nil {
		return err
	}
	installation, err := os.CreateTemp("", "aries-calico-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(installation.Name())
	_ = installation.Close()
	if err := utils.WriteFile(installation.Name(), CalicoInstallation(kube.PodCIDR), 0o600); err != nil {
		return err
	}
	_, err = utils.ExecShellCmd(kubectlAdmin+" apply -f %s", utils.Quote(installation.Name()))
	return err
}

// CalicoInstallation renders the operator resources for one IPv4 pool.
func CalicoInstallation(podCIDR string) string {
	return strings.TrimLeft(fmt.Sprintf(`
apiVersion: operator.tigera.io/v1
kind: Installation
metadata:
  name: default
spec:
  calicoNetwork:
    ipPools:
      - name: default-ipv4-ippool
        cidr: %s
        encapsulation: VXLANCrossSubnet
---
apiVersion: operator.tigera.io/v1
kind: APIServer
metadata:
  name: default
spec: {}
`, podCIDR), "\n")
}

// writeJoinFile mints a fresh 24h join command and saves it for manual joins.
func writeJoinFile(system configs.System) (JoinCommand, error) {
	raw, err := utils.ExecShellCmd("kubeadm token create --print-join-command")
	if err != nil {
		return JoinCommand{}, err
	}
	join, err := ParseJoinCommand(lastLine(raw))
	if err != nil {
		return JoinCommand{}, err
	}
	if _, err := utils.ExecShellCmd("install -m 0700 -d \"$(dirname %s)\"", utils.Quote(system.JoinFile)); err != nil {
		return JoinCommand{}, err
	}
	content := fmt.Sprintf("#!/usr/bin/env bash\n# Generated by aries-setup setup_master_node on %s.\n"+
		"# Valid for 24h; regenerate with: kubeadm token create --print-join-command\n%s\n",
		time.Now().Format(time.RFC3339), join.String())
	return join, utils.WriteFile(system.JoinFile, content, 0o700)
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
