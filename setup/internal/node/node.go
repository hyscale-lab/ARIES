// Package node prepares a single machine to become a Kubernetes node: the
// work every node needs regardless of role, and tearing that work back down.
package node

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

// Host describes the machine a node-level step runs on.
type Host struct {
	ID       string // os-release ID, e.g. "ubuntu"
	Like     string // os-release ID_LIKE
	Codename string // os-release VERSION_CODENAME
	Version  string // os-release VERSION_ID
	Pkg      string // "apt" or "dnf"
	DebArch  string // "amd64" or "arm64"
}

// DetectHost reads /etc/os-release. The architecture comes from the binary
// itself: create_cluster builds aries-setup for the node's architecture and
// checks `uname -m` against it before shipping.
func DetectHost() (Host, error) {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return Host{}, fmt.Errorf("cannot read /etc/os-release; unsupported host: %w", err)
	}
	defer file.Close()
	return ParseOSRelease(bufio.NewScanner(file), runtime.GOARCH)
}

// ParseOSRelease maps os-release content and a GOARCH to a Host.
func ParseOSRelease(scanner *bufio.Scanner, goarch string) (Host, error) {
	values := map[string]string{}
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		values[key] = strings.Trim(value, `"'`)
	}
	if err := scanner.Err(); err != nil {
		return Host{}, err
	}

	host := Host{ID: values["ID"], Like: values["ID_LIKE"], Codename: values["VERSION_CODENAME"], Version: values["VERSION_ID"]}
	family := host.ID + " " + host.Like
	switch {
	case strings.Contains(family, "debian") || strings.Contains(family, "ubuntu"):
		host.Pkg = "apt"
	case strings.Contains(family, "rhel") || strings.Contains(family, "fedora") || strings.Contains(family, "centos"):
		host.Pkg = "dnf"
	default:
		return Host{}, fmt.Errorf("unsupported distribution %q; expected a Debian- or RHEL-family host", host.ID)
	}
	switch goarch {
	case "amd64", "arm64":
		host.DebArch = goarch
	default:
		return Host{}, fmt.Errorf("unsupported architecture %q; kubeadm nodes need amd64 or arm64", goarch)
	}
	return host, nil
}

// SetupNode installs and configures everything a node needs before kubeadm
// can init or join it. Every step is safe to re-run.
func SetupNode(kube configs.Kube, system configs.System) error {
	if err := utils.RequireRoot(); err != nil {
		return err
	}
	host, err := DetectHost()
	if err != nil {
		return err
	}
	utils.InfoPrintf("host: %s %s (%s), package manager: %s", host.ID, host.Version, host.DebArch, host.Pkg)

	minor, err := ResolveMinor(kube, system)
	if err != nil {
		return err
	}

	steps := []struct {
		name string
		run  func() error
	}{
		{"Installing prerequisites", func() error { return installPrerequisites(host) }},
		{"Disabling swap", disableSwap},
		{"Configuring kernel modules and sysctls", configureKernel},
		{"Installing containerd", func() error { return installContainerd(host, system) }},
		{fmt.Sprintf("Installing kubelet, kubeadm and kubectl (%s)", minor), func() error {
			return installKubePackages(host, kube, system, minor)
		}},
	}
	for _, step := range steps {
		utils.WaitPrintf("%s", step.name)
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w", strings.ToLower(step.name), err)
		}
	}
	utils.SuccessPrintf("node prepared")
	return nil
}

// ResolveMinor returns the package channel to install from. An explicit
// channel wins, then the channel of a pinned version, then the upstream
// stable pointer.
func ResolveMinor(kube configs.Kube, system configs.System) (string, error) {
	switch {
	case kube.K8sMinor != "":
		return kube.K8sMinor, nil
	case kube.K8sVersion != "":
		return configs.MinorOf(kube.K8sVersion), nil
	}
	stable, err := utils.ExecShellCmd("curl -fsSL --retry 3 %s", utils.Quote(system.K8sStableURL))
	if err != nil {
		return "", fmt.Errorf("cannot reach %s; set k8s_minor in kube.json to install without it: %w", system.K8sStableURL, err)
	}
	minor := configs.MinorOf(stable)
	if minor == "" {
		return "", fmt.Errorf("unexpected stable release %q from %s", stable, system.K8sStableURL)
	}
	return minor, nil
}

func installPrerequisites(host Host) error {
	if host.Pkg == "apt" {
		_, err := utils.ExecShellCmd("DEBIAN_FRONTEND=noninteractive apt-get update -qq && " +
			"DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl gnupg apt-transport-https")
		return err
	}
	_, err := utils.ExecShellCmd("dnf install -y -q ca-certificates curl gnupg2 iproute-tc")
	return err
}

// disableSwap is required: the kubelet refuses to start with swap enabled
// unless configured for it, and kubeadm preflight rejects the node first.
func disableSwap() error {
	active, err := utils.ExecShellCmd("swapon --show --noheadings | wc -l")
	if err != nil {
		return err
	}
	if strings.TrimSpace(active) != "0" {
		if _, err := utils.ExecShellCmd("swapoff -a"); err != nil {
			return err
		}
	}
	// Comment out swap entries so the node stays swap-free across a reboot.
	_, err = utils.ExecShellCmd(`if grep -qE '^[^#].*\sswap\s' /etc/fstab 2>/dev/null; then ` +
		`cp /etc/fstab "/etc/fstab.aries.bak.$(date +%s)" && ` +
		`sed -i -E 's|^([^#].*\sswap\s.*)$|# \1  # disabled by aries-setup|' /etc/fstab; fi`)
	return err
}

func configureKernel() error {
	if err := utils.WriteFile("/etc/modules-load.d/k8s.conf", "overlay\nbr_netfilter\n", 0o644); err != nil {
		return err
	}
	if _, err := utils.ExecShellCmd("modprobe overlay && modprobe br_netfilter"); err != nil {
		return err
	}
	sysctls := "net.bridge.bridge-nf-call-iptables  = 1\n" +
		"net.bridge.bridge-nf-call-ip6tables = 1\n" +
		"net.ipv4.ip_forward                 = 1\n"
	if err := utils.WriteFile("/etc/sysctl.d/99-kubernetes.conf", sysctls, 0o644); err != nil {
		return err
	}
	_, err := utils.ExecShellCmd("sysctl --system")
	return err
}

// installContainerd takes containerd.io from Docker's repository, which ships
// a build matching the CRI version kubeadm expects, then switches the cgroup
// driver to systemd to agree with the kubelet default.
func installContainerd(host Host, system configs.System) error {
	if _, err := utils.ExecShellCmd("command -v containerd"); err == nil {
		version, _ := utils.ExecShellCmd("containerd --version | awk '{print $3}'")
		utils.InfoPrintf("containerd already installed (%s)", version)
	} else if err := addContainerdRepoAndInstall(host, system); err != nil {
		return err
	}

	if _, err := utils.ExecShellCmd("mkdir -p /etc/containerd && containerd config default > /etc/containerd/config.toml"); err != nil {
		return err
	}
	// The pattern matches both the v2 and v3 plugin layouts, so it holds across
	// containerd major versions.
	if _, err := utils.ExecShellCmd(`sed -i 's/^\( *\)SystemdCgroup = false/\1SystemdCgroup = true/' /etc/containerd/config.toml`); err != nil {
		return err
	}
	if _, err := utils.ExecShellCmd("grep -q 'SystemdCgroup = true' /etc/containerd/config.toml"); err != nil {
		utils.WarnPrintf("SystemdCgroup not found in containerd config; verify the cgroup driver manually")
	}
	if _, err := utils.ExecShellCmd("systemctl daemon-reload && systemctl enable --now containerd && systemctl restart containerd"); err != nil {
		return err
	}
	// crictl ships with the kube packages; pointing it at containerd makes
	// on-node debugging work without repeating the endpoint flag.
	crictl := fmt.Sprintf("runtime-endpoint: %s\nimage-endpoint: %s\ntimeout: 10\n", system.CRISocket, system.CRISocket)
	return utils.WriteFile("/etc/crictl.yaml", crictl, 0o644)
}

func addContainerdRepoAndInstall(host Host, system configs.System) error {
	if host.Pkg == "dnf" {
		repo := "centos"
		if host.ID == "fedora" {
			repo = "fedora"
		}
		url := fmt.Sprintf("%s/%s/docker-ce.repo", system.DockerRepo, repo)
		_, err := utils.ExecShellCmd("curl -fsSL --retry 3 -o /etc/yum.repos.d/docker-ce.repo %s && dnf install -y -q containerd.io", utils.Quote(url))
		return err
	}

	distro := host.ID
	if distro != "ubuntu" && distro != "debian" {
		distro = "debian"
		if strings.Contains(host.Like, "ubuntu") {
			distro = "ubuntu"
		}
	}
	if host.Codename == "" {
		return fmt.Errorf("os-release has no VERSION_CODENAME; cannot select a Docker apt suite")
	}
	gpgURL := fmt.Sprintf("%s/%s/gpg", system.DockerRepo, distro)
	if _, err := utils.ExecShellCmd("install -m 0755 -d /etc/apt/keyrings && "+
		"curl -fsSL --retry 3 %s | gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg && "+
		"chmod a+r /etc/apt/keyrings/docker.gpg", utils.Quote(gpgURL)); err != nil {
		return err
	}
	source := fmt.Sprintf("deb [arch=%s signed-by=/etc/apt/keyrings/docker.gpg] %s/%s %s stable\n",
		host.DebArch, system.DockerRepo, distro, host.Codename)
	if err := utils.WriteFile("/etc/apt/sources.list.d/docker.list", source, 0o644); err != nil {
		return err
	}
	_, err := utils.ExecShellCmd("DEBIAN_FRONTEND=noninteractive apt-get update -qq && " +
		"DEBIAN_FRONTEND=noninteractive apt-get install -y -qq containerd.io")
	return err
}

func installKubePackages(host Host, kube configs.Kube, system configs.System, minor string) error {
	if version, err := utils.ExecShellCmd("kubeadm version -o short"); err == nil {
		utils.InfoPrintf("kubeadm already installed (%s)", version)
		return nil
	}
	repo := fmt.Sprintf("%s/%s", system.K8sPackageRepo, minor)

	if host.Pkg == "apt" {
		if _, err := utils.ExecShellCmd("install -m 0755 -d /etc/apt/keyrings && "+
			"curl -fsSL --retry 3 %s | gpg --dearmor --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg && "+
			"chmod a+r /etc/apt/keyrings/kubernetes-apt-keyring.gpg", utils.Quote(repo+"/deb/Release.key")); err != nil {
			return err
		}
		source := fmt.Sprintf("deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] %s/deb/ /\n", repo)
		if err := utils.WriteFile("/etc/apt/sources.list.d/kubernetes.list", source, 0o644); err != nil {
			return err
		}
		packages := "kubelet kubeadm kubectl"
		if kube.K8sVersion != "" {
			// pkgs.k8s.io debs carry a packaging suffix such as -1.1.
			v := kube.K8sVersion
			packages = utils.QuoteAll("kubelet="+v+"-*", "kubeadm="+v+"-*", "kubectl="+v+"-*")
		}
		if _, err := utils.ExecShellCmd("DEBIAN_FRONTEND=noninteractive apt-get update -qq && "+
			"DEBIAN_FRONTEND=noninteractive apt-get install -y -qq %s && "+
			"apt-mark hold kubelet kubeadm kubectl", packages); err != nil {
			return err
		}
	} else {
		repoFile := fmt.Sprintf("[kubernetes]\nname=Kubernetes\nbaseurl=%s/rpm/\nenabled=1\ngpgcheck=1\n"+
			"gpgkey=%s/rpm/repodata/repomd.xml.key\nexclude=kubelet kubeadm kubectl cri-tools kubernetes-cni\n", repo, repo)
		if err := utils.WriteFile("/etc/yum.repos.d/kubernetes.repo", repoFile, 0o644); err != nil {
			return err
		}
		packages := "kubelet kubeadm kubectl"
		if kube.K8sVersion != "" {
			v := kube.K8sVersion
			packages = utils.QuoteAll("kubelet-"+v, "kubeadm-"+v, "kubectl-"+v)
		}
		if _, err := utils.ExecShellCmd("dnf install -y -q --disableexcludes=kubernetes %s", packages); err != nil {
			return err
		}
	}
	_, err := utils.ExecShellCmd("systemctl enable --now kubelet")
	return err
}

// Firewall roles.
const (
	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"
)

// FirewallPorts lists the TCP and UDP ports kubeadm needs for a role and CNI.
func FirewallPorts(role, cni string) (tcp, udp []string) {
	switch role {
	case RoleControlPlane:
		tcp = []string{"6443", "2379:2380", "10250", "10256", "10257", "10259", "30000:32767"}
	default:
		tcp = []string{"10250", "10256", "30000:32767"}
	}
	switch cni {
	case configs.CNIFlannel:
		udp = []string{"8472"}
	case configs.CNICalico:
		tcp = append(tcp, "179")
	}
	return tcp, udp
}

// OpenFirewall opens the role's ports in an active ufw or firewalld, when
// kube.json asks for it.
func OpenFirewall(role string, kube configs.Kube) error {
	tcp, udp := FirewallPorts(role, kube.CNI)
	if !kube.OpenFirewall {
		ports := "tcp " + strings.Join(tcp, " ")
		if len(udp) > 0 {
			ports += ", udp " + strings.Join(udp, " ")
		}
		utils.InfoPrintf("firewall untouched (set open_firewall to open %s)", ports)
		return nil
	}
	utils.WaitPrintf("Opening %s firewall ports", role)
	if _, err := utils.ExecShellCmd("command -v ufw >/dev/null && ufw status | grep -q '^Status: active'"); err == nil {
		for _, port := range tcp {
			if _, err := utils.ExecShellCmd("ufw allow %s", utils.Quote(port+"/tcp")); err != nil {
				return err
			}
		}
		for _, port := range udp {
			if _, err := utils.ExecShellCmd("ufw allow %s", utils.Quote(port+"/udp")); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := utils.ExecShellCmd("command -v firewall-cmd >/dev/null && firewall-cmd --state"); err == nil {
		for _, port := range tcp {
			// firewalld spells ranges with a dash.
			if _, err := utils.ExecShellCmd("firewall-cmd --permanent --add-port=%s", utils.Quote(strings.ReplaceAll(port, ":", "-")+"/tcp")); err != nil {
				return err
			}
		}
		for _, port := range udp {
			if _, err := utils.ExecShellCmd("firewall-cmd --permanent --add-port=%s", utils.Quote(port+"/udp")); err != nil {
				return err
			}
		}
		_, err := utils.ExecShellCmd("firewall-cmd --reload")
		return err
	}
	utils.InfoPrintf("no active ufw or firewalld; nothing to open")
	return nil
}

// ResetNode tears a node back to a pre-kubeadm state so it can be bootstrapped
// again. It destroys etcd data on a control plane, every pod, the CNI
// configuration and local kubeconfigs, but leaves containerd and the kube
// packages installed.
func ResetNode(system configs.System) error {
	if err := utils.RequireRoot(); err != nil {
		return err
	}
	hostname, _ := os.Hostname()

	utils.WaitPrintf("Running kubeadm reset on %s", hostname)
	if _, err := utils.ExecShellCmd("kubeadm reset -f --cri-socket %s", utils.Quote(system.CRISocket)); err != nil {
		utils.WarnPrintf("kubeadm reset reported an error; continuing with manual cleanup")
	}

	utils.WaitPrintf("Removing CNI, kubelet and kubeconfig state")
	if _, err := utils.ExecShellCmd("rm -rf /etc/cni/net.d /var/lib/cni /var/lib/kubelet/* /etc/kubernetes %s /root/.kube/config",
		utils.Quote(system.JoinFile)); err != nil {
		return err
	}
	if home := SudoUserHome(); home != "" {
		if _, err := utils.ExecShellCmd("rm -f %s", utils.Quote(home+"/.kube/config")); err != nil {
			return err
		}
	}

	// kubeadm reset leaves the CNI's interfaces behind.
	for _, link := range []string{"cni0", "flannel.1", "vxlan.calico"} {
		if _, err := utils.ExecShellCmd("ip link show %s", link); err == nil {
			utils.InfoPrintf("deleting stale interface %s", link)
			if _, err := utils.ExecShellCmd("ip link delete %s", link); err != nil {
				return err
			}
		}
	}

	utils.WaitPrintf("Flushing rules left by kube-proxy")
	_, _ = utils.ExecShellCmd("command -v iptables >/dev/null && iptables -F && iptables -t nat -F && iptables -t mangle -F && iptables -X")
	_, _ = utils.ExecShellCmd("command -v ipvsadm >/dev/null && ipvsadm -C")
	_, _ = utils.ExecShellCmd("systemctl restart containerd")

	utils.SuccessPrintf("node reset; run setup_master_node or setup_worker to bootstrap again")
	return nil
}

// SudoUserHome returns the home directory of the user who invoked sudo, or ""
// when run as root directly.
func SudoUserHome() string {
	user := os.Getenv("SUDO_USER")
	if user == "" || user == "root" {
		return ""
	}
	home, err := utils.ExecShellCmd("getent passwd %s | cut -d: -f6", utils.Quote(user))
	if err != nil {
		return ""
	}
	return home
}
