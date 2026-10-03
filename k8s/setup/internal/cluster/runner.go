package cluster

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/k8s/setup/internal/configs"
	"github.com/hyscale-lab/aries/k8s/setup/internal/utils"
)

// The runner's identity, as the ARIES chart creates it (fullnameOverride
// "aries", serviceAccount.name "aries"). setup_runner builds the runner
// host's kubeconfig from this account's token, not from admin.conf.
const (
	runnerServiceAccount = "aries"
	runnerTokenSecret    = runnerServiceAccount + "-token"
	defaultRunnerDir     = "aries"
	// runnerKubeconfig is written beside the staged directories, 0600.
	runnerKubeconfig = "kubeconfig"
)

var (
	// runnerStaged are the directories setup_runner replaces on every run.
	// Everything else under the runner directory — runs/, .cache/,
	// DEEPSEEK_API.key and the kubeconfig — is left alone.
	runnerStaged = []string{"bin", "profiles", "configs"}
	kubeVersion  = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

// RunnerOptions configure setup_runner.
type RunnerOptions struct {
	Cluster configs.Cluster
	// Namespace is the ARIES chart's namespace, where the runner's
	// ServiceAccount and its token live.
	Namespace string
	// RepoRoot is the ARIES checkout the runner is built and staged from.
	RepoRoot string
}

// SetupRunner prepares the runner host, which is deliberately not a cluster
// node. From the operator's machine it:
//
//  1. checks the host is reachable, has sudo, and is not in the cluster
//  2. installs git and a kubectl matching the cluster's server version
//  3. builds bin/aries and bin/aries-ssh for the host and stages them with
//     profiles/ and configs/ under ~/<runner_dir>
//  4. writes ~/<runner_dir>/kubeconfig for the chart's "aries" ServiceAccount,
//     so the runner holds the namespaced Role rather than cluster-admin
//  5. confirms, from the host, that it can see the bridge pod and cannot
//     create pods outside its namespace
//
// The model key is never copied: put DEEPSEEK_API.key there yourself.
func SetupRunner(opts RunnerOptions) error {
	cluster := opts.Cluster
	if cluster.Runner == "" {
		return errors.New(`cluster.json names no "runner" host`)
	}
	dir := cluster.RunnerDir
	if dir == "" {
		dir = defaultRunnerDir
	}
	ssh := newSSH(cluster)
	target := cluster.Runner

	utils.WaitPrintf("Preflight: runner %s", target)
	if _, err := ssh.run(target, "true"); err != nil {
		return fmt.Errorf("cannot ssh to runner %s non-interactively: %w", target, err)
	}
	if _, err := ssh.run(target, "sudo -n true"); err != nil {
		return fmt.Errorf("no passwordless sudo on runner %s; needed to install kubectl and git", target)
	}
	machine, err := ssh.run(target, "uname -m")
	if err != nil {
		return err
	}
	arch := GoArch(machine)
	if err := requireOutsideCluster(ssh, cluster); err != nil {
		return err
	}

	server, err := clusterFacts(ssh, cluster.Master)
	if err != nil {
		return err
	}
	kubeconfig, err := runnerKubeconfigFor(ssh, cluster.Master, opts.Namespace, server.url)
	if err != nil {
		return err
	}

	utils.WaitPrintf("Installing git and kubectl %s (linux/%s) on the runner", server.version, arch)
	if _, err := ssh.run(target, installRunnerTools(server.version, arch)); err != nil {
		return err
	}

	stage, err := buildRunnerStage(opts.RepoRoot, arch)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	utils.WaitPrintf("Staging the runner to %s:~/%s", target, dir)
	remote := "mkdir -p " + utils.Quote(dir) + " && cd " + utils.Quote(dir) +
		" && rm -rf " + strings.Join(runnerStaged, " ") + " && tar -xf -"
	if _, err := utils.ExecShellCmd(ssh.stageLine(target, stage, remote)); err != nil {
		return err
	}

	// The token is written through a 0600 local file piped over ssh, so it is
	// never on a command line or in the setup log.
	if err := writeRunnerKubeconfig(ssh, target, dir, kubeconfig); err != nil {
		return err
	}

	utils.WaitPrintf("Checking the runner's cluster access from %s", target)
	kubectl := "KUBECONFIG=" + utils.Quote(dir+"/"+runnerKubeconfig) + " kubectl"
	bridge, err := ssh.run(target, kubectl+" -n "+utils.Quote(opts.Namespace)+" get pods -l app.kubernetes.io/name=aries-bridge -o name")
	if err != nil {
		return fmt.Errorf("the runner cannot reach the API server at %s: %w", server.url, err)
	}
	if strings.TrimSpace(bridge) == "" {
		utils.WarnPrintf("no aries-bridge pod in namespace %s yet; profiles with bridge.deployment kubernetes will fail preflight until it is up", opts.Namespace)
	} else {
		utils.InfoPrintf("bridge pod: %s", strings.TrimSpace(bridge))
	}
	if answer, _ := ssh.run(target, kubectl+" auth can-i create pods -n kube-system"); strings.TrimSpace(answer) != "no" {
		return fmt.Errorf("the runner's kubeconfig can create pods in kube-system; it must only hold the namespaced Role")
	}

	if _, err := ssh.run(target, "test -f "+utils.Quote(dir+"/DEEPSEEK_API.key")); err != nil {
		utils.WarnPrintf("no %s/DEEPSEEK_API.key on the runner; copy it there with mode 0600 before a DeepSeek run", dir)
	}
	utils.SuccessPrintf("runner is ready on %s", target)
	utils.InfoPrintf("  ssh %s", target)
	utils.InfoPrintf("  cd ~/%s && export KUBECONFIG=$PWD/%s", dir, runnerKubeconfig)
	utils.InfoPrintf("  ./bin/aries profiles/hermes-tb2-fix-git-deepseek-k8s-pods.json")
	return nil
}

// requireOutsideCluster refuses a runner host that is a cluster node, or still
// runs a kubelet from an earlier join: the point of the split is that the
// runner does not share a machine with the pods it measures.
func requireOutsideCluster(ssh sshClient, cluster configs.Cluster) error {
	name, err := nodeName(ssh, cluster.Runner)
	if err != nil {
		return err
	}
	nodes, err := ssh.run(cluster.Master, "sudo "+kubectlAdmin+" get nodes -o jsonpath='{.items[*].metadata.name}'")
	if err != nil {
		return err
	}
	for _, node := range strings.Fields(nodes) {
		if strings.EqualFold(node, name) {
			return fmt.Errorf("runner %s is cluster node %s; drain and delete it (kubectl delete node %s), then reset_node --yes on it", cluster.Runner, node, node)
		}
	}
	if _, err := ssh.run(cluster.Runner, "systemctl is-active --quiet kubelet"); err == nil {
		return fmt.Errorf("runner %s still runs a kubelet; run reset_node --yes on it first", cluster.Runner)
	}
	return nil
}

type serverFacts struct {
	url     string
	version string
}

// clusterFacts reads the API server's address and version from the master.
// kubectl on the runner is matched to that version, within the supported skew.
func clusterFacts(ssh sshClient, master string) (serverFacts, error) {
	url, err := ssh.run(master, "sudo "+kubectlAdmin+" config view --minify -o jsonpath='{.clusters[0].cluster.server}'")
	if err != nil {
		return serverFacts{}, err
	}
	raw, err := ssh.run(master, "sudo "+kubectlAdmin+" version -o json")
	if err != nil {
		return serverFacts{}, err
	}
	var version struct {
		Server struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal([]byte(raw), &version); err != nil {
		return serverFacts{}, fmt.Errorf("parse the server version: %w", err)
	}
	facts := serverFacts{url: strings.TrimSpace(url), version: version.Server.GitVersion}
	if !strings.HasPrefix(facts.url, "https://") || !kubeVersion.MatchString(facts.version) {
		return serverFacts{}, fmt.Errorf("unexpected API server %q or version %q", facts.url, facts.version)
	}
	return facts, nil
}

// runnerKubeconfigFor builds a kubeconfig for the runner's ServiceAccount
// from its token Secret. The token controller fills the Secret in shortly
// after the chart creates it, so this waits briefly for it.
func runnerKubeconfigFor(ssh sshClient, master, namespace, server string) ([]byte, error) {
	read := func(key string) (string, error) {
		return ssh.runSecret(master, "sudo "+kubectlAdmin+" -n "+utils.Quote(namespace)+" get secret "+runnerTokenSecret+
			" -o jsonpath="+utils.Quote("{.data."+key+"}"))
	}
	var token, ca string
	for attempt := 0; ; attempt++ {
		var err error
		token, err = read("token")
		if err != nil {
			return nil, fmt.Errorf("read secret %s/%s; deploy the ARIES chart first (deploy_aries): %w", namespace, runnerTokenSecret, err)
		}
		if ca, err = read(`ca\.crt`); err != nil {
			return nil, err
		}
		if token != "" && ca != "" {
			break
		}
		if attempt == 15 {
			return nil, fmt.Errorf("secret %s/%s was never filled in by the token controller", namespace, runnerTokenSecret)
		}
		time.Sleep(2 * time.Second)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return nil, fmt.Errorf("decode the runner token: %w", err)
	}
	return RunnerKubeconfig(server, strings.TrimSpace(ca), string(decoded), namespace)
}

// RunnerKubeconfig renders a kubeconfig for the runner. JSON is valid
// kubeconfig, and marshalling it means no value ever needs escaping.
func RunnerKubeconfig(server, caData, token, namespace string) ([]byte, error) {
	if server == "" || caData == "" || token == "" || namespace == "" {
		return nil, errors.New("runner kubeconfig needs a server, a CA, a token and a namespace")
	}
	config := map[string]any{
		"apiVersion": "v1", "kind": "Config", "current-context": "aries-runner",
		"clusters": []any{map[string]any{"name": "aries", "cluster": map[string]any{
			"server": server, "certificate-authority-data": caData,
		}}},
		"users": []any{map[string]any{"name": "aries-runner", "user": map[string]any{"token": token}}},
		"contexts": []any{map[string]any{"name": "aries-runner", "context": map[string]any{
			"cluster": "aries", "user": "aries-runner", "namespace": namespace,
		}}},
	}
	return json.MarshalIndent(config, "", "  ")
}

func writeRunnerKubeconfig(ssh sshClient, target, dir string, content []byte) error {
	local, err := os.CreateTemp("", "aries-runner-kubeconfig-")
	if err != nil {
		return err
	}
	defer os.Remove(local.Name())
	if err := local.Chmod(0o600); err != nil {
		local.Close()
		return err
	}
	if _, err := local.Write(content); err != nil {
		local.Close()
		return err
	}
	if err := local.Close(); err != nil {
		return err
	}
	path := dir + "/" + runnerKubeconfig
	remote := "umask 077 && cat > " + utils.Quote(path+".tmp") + " && mv " + utils.Quote(path+".tmp") + " " + utils.Quote(path)
	_, err = utils.ExecShellCmd("ssh %s %s %s < %s", utils.QuoteAll(ssh.options...), utils.Quote(target), utils.Quote(remote), utils.Quote(local.Name()))
	return err
}

// installRunnerTools renders the remote script that installs git and a
// checksum-verified kubectl, skipping kubectl when the right one is present.
func installRunnerTools(version, arch string) string {
	base := "https://dl.k8s.io/release/" + version + "/bin/linux/" + arch + "/kubectl"
	return strings.Join([]string{
		"set -e",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git curl ca-certificates >/dev/null",
		"if kubectl version --client -o json 2>/dev/null | grep -q " + utils.Quote(`"gitVersion": "`+version+`"`) + "; then exit 0; fi",
		"work=$(mktemp -d)",
		"curl -fsSL --retry 3 -o \"$work/kubectl\" " + utils.Quote(base),
		"curl -fsSL --retry 3 -o \"$work/kubectl.sha256\" " + utils.Quote(base+".sha256"),
		`echo "$(cat "$work/kubectl.sha256")  $work/kubectl" | sha256sum -c - >/dev/null`,
		`sudo install -m 0755 "$work/kubectl" /usr/local/bin/kubectl`,
		`rm -rf "$work"`,
	}, "\n")
}

// buildRunnerStage builds the runner binaries for arch and lays out what the
// runner host receives.
func buildRunnerStage(repoRoot, arch string) (string, error) {
	stage, err := os.MkdirTemp("", "aries-runner-stage-")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		os.RemoveAll(stage)
		return "", err
	}
	utils.WaitPrintf("Building the runner for linux/%s", arch)
	bin := filepath.Join(stage, "bin")
	for _, command := range []string{"aries", "aries-ssh"} {
		if _, err := utils.ExecShellCmd("cd %s && CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -trimpath -o %s ./cmd/%s",
			utils.Quote(repoRoot), utils.Quote(arch), utils.Quote(filepath.Join(bin, command)), command); err != nil {
			return fail(err)
		}
	}
	for _, dir := range []string{"profiles", "configs"} {
		if _, err := utils.ExecShellCmd("cp -Rp %s %s", utils.Quote(filepath.Join(repoRoot, dir)), utils.Quote(stage)); err != nil {
			return fail(err)
		}
	}
	return stage, nil
}
