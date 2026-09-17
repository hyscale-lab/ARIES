package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

// remoteDir is where create_cluster stages aries-setup on each node, relative
// to the SSH user's home.
const remoteDir = ".aries-setup"

// remoteChartsDir is the staged Helm charts directory inside remoteDir. It
// mirrors the repository's k8s/ tree, so --charts-dir points here on a node.
const remoteChartsDir = "charts"

// CreateOptions configures create_cluster.
type CreateOptions struct {
	// ConfigsDir holds kube.json and system.json, which are shipped to every
	// node, and cluster.json, which is not.
	ConfigsDir string
	// ChartsDir holds the Helm charts (k8s/ in the repository). Only the charts
	// being deployed are shipped, and only to the master.
	ChartsDir string
	// AriesValuesFiles is aries_config.json's values_files, checked here so a
	// missing secret.yaml fails before any node is touched.
	AriesValuesFiles []string
	Cluster          configs.Cluster
	// NodeBinary is a prebuilt linux aries-setup. Empty builds one from this
	// module for NodeArch, so a stale binary is never shipped by accident.
	NodeBinary string
	NodeArch   string
	// KubeconfigOut is where fetch_kubeconfig writes the admin kubeconfig.
	KubeconfigOut string
	CheckOnly     bool
	Reset         bool
	SkipMaster    bool
	// SkipWorkers leaves the existing workers untouched: no preflight, no
	// token, no join. With SkipMaster it reduces a run to the role labels and
	// the chart deployments, which is how Prometheus or ARIES is re-installed
	// on a cluster that already exists. Without it, setup_worker refuses on a
	// node that already belongs to a cluster — correctly, since joining a live
	// node again would disrupt it.
	SkipWorkers bool
}

// CreateCluster builds the whole cluster from the operator's machine. It runs
// the same on-node subcommands a person would run by hand, over SSH:
//
//	every node   setup_node
//	master       setup_master_node
//	workers      setup_worker --join <cmd>   (in parallel)
//
// then labels and taints the role pools.
func CreateCluster(opts CreateOptions) error {
	ssh := newSSH(opts.Cluster)
	workers := Workers(opts.Cluster)
	if opts.SkipWorkers {
		// Do not preflight or contact nodes that will not be touched: with
		// --skip-master --skip-workers this runs only the deploy steps on the
		// control plane, and a worker that happens to be down must not block
		// re-installing Prometheus.
		workers = nil
	}

	utils.WaitPrintf("Preflight: %d node(s)", len(workers)+1)
	if err := preflightNode(ssh, opts.Cluster.Master, "master", opts.NodeArch); err != nil {
		return err
	}
	for _, node := range workers {
		if err := preflightNode(ssh, node, RoleOf(opts.Cluster, node), opts.NodeArch); err != nil {
			return err
		}
	}
	if opts.CheckOnly {
		utils.SuccessPrintf("preflight passed; --check requested, nothing changed")
		return nil
	}

	stage, err := buildStage(opts)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	if opts.Reset {
		// Workers go first and sequentially. They hold leases on the control
		// plane, so resetting the API server first can leave a worker's
		// kubeadm reset blocked on a server that no longer answers. A failed
		// reset only warns: a node never in a cluster has nothing to remove.
		utils.WaitPrintf("Resetting every node (--reset)")
		for _, node := range append(append([]string{}, workers...), opts.Cluster.Master) {
			utils.InfoPrintf("reset %s", node)
			if err := ssh.stage(node, stage); err != nil {
				return err
			}
			if _, err := ssh.run(node, onNode("reset_node", "--yes")); err != nil {
				utils.WarnPrintf("reset reported errors on %s; continuing", node)
			}
		}
	}

	if opts.SkipMaster {
		utils.InfoPrintf("--skip-master: using the existing control plane on %s", opts.Cluster.Master)
	} else {
		utils.WaitPrintf("Setting up the control plane on %s (several minutes)", opts.Cluster.Master)
		if err := ssh.stage(opts.Cluster.Master, stage); err != nil {
			return err
		}
		for _, sub := range []string{"setup_node", "setup_master_node"} {
			if err := ssh.stream(opts.Cluster.Master, onNode(sub)); err != nil {
				return fmt.Errorf("%s on master %s: %w", sub, opts.Cluster.Master, err)
			}
		}
	}

	if opts.SkipWorkers {
		// No token is minted either: freshJoinCommand creates a 24h bootstrap
		// credential, and there is no reason to issue one nobody will use.
		utils.InfoPrintf("--skip-workers: leaving the existing workers alone")
	} else {
		join, err := freshJoinCommand(ssh, opts.Cluster.Master)
		if err != nil {
			return err
		}
		if err := joinWorkers(ssh, stage, workers, join); err != nil {
			return err
		}
	}
	if err := applyRoles(ssh, opts.Cluster); err != nil {
		return err
	}

	// Normally the master was staged for setup_master_node. Re-staging it would
	// delete that step's node log, so only stage when it was skipped.
	if (opts.Cluster.DeployPrometheus || opts.Cluster.DeployAries) && opts.SkipMaster {
		if err := ssh.stage(opts.Cluster.Master, stage); err != nil {
			return err
		}
	}
	if opts.Cluster.DeployPrometheus {
		utils.WaitPrintf("Setting up Prometheus and Grafana from %s", opts.Cluster.Master)
		if err := ssh.stream(opts.Cluster.Master, onNode("setup_prometheus")); err != nil {
			return fmt.Errorf("setup_prometheus on master %s: %w", opts.Cluster.Master, err)
		}
	}
	if opts.Cluster.DeployAries {
		utils.WaitPrintf("Deploying ARIES from %s", opts.Cluster.Master)
		if err := ssh.stream(opts.Cluster.Master, onNode("setup_aries")); err != nil {
			return fmt.Errorf("setup_aries on master %s: %w", opts.Cluster.Master, err)
		}
	}

	nodes, err := ssh.run(opts.Cluster.Master, "sudo "+kubectlAdmin+" get nodes -o wide -L "+RoleLabel)
	if err == nil {
		utils.InfoPrintf("\n%s", nodes)
	}
	if opts.Cluster.FetchKubeconfig {
		if err := fetchKubeconfig(ssh, opts.Cluster.Master, opts.KubeconfigOut); err != nil {
			return err
		}
	}
	utils.SuccessPrintf("cluster is up; nodes report Ready once the CNI DaemonSet schedules on each")
	return nil
}

// onNode renders an aries-setup invocation inside the staged directory. Both
// directories are relative to remoteDir, which the command cds into first.
func onNode(sub string, args ...string) string {
	line := fmt.Sprintf("cd %s && sudo ./aries-setup --configs-dir configs --charts-dir %s %s",
		remoteDir, remoteChartsDir, sub)
	if len(args) > 0 {
		line += " " + utils.QuoteAll(args...)
	}
	return line
}

func preflightNode(ssh sshClient, node, role, arch string) error {
	if _, err := ssh.run(node, "true"); err != nil {
		return fmt.Errorf("cannot ssh to %s %s non-interactively; check the address, your key and ssh-agent: %w", role, node, err)
	}
	if _, err := ssh.run(node, "sudo -n true"); err != nil {
		return fmt.Errorf("no passwordless sudo on %s %s; required because setup runs unattended", role, node)
	}
	machine, err := ssh.run(node, "uname -m")
	if err != nil {
		return err
	}
	if got := GoArch(machine); got != arch {
		return fmt.Errorf("%s %s is %s but the node binary targets %s; pass --node-arch", role, node, machine, arch)
	}
	utils.InfoPrintf("%-8s %s — reachable, sudo ok, %s", role, node, machine)
	return nil
}

// GoArch maps `uname -m` to a GOARCH.
func GoArch(machine string) string {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return strings.TrimSpace(machine)
}

// buildStage assembles what each node receives: the linux binary plus
// kube.json and system.json. cluster.json stays behind; it names every host
// and is only needed here.
func buildStage(opts CreateOptions) (string, error) {
	stage, err := os.MkdirTemp("", "aries-setup-stage-")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		os.RemoveAll(stage)
		return "", err
	}
	if err := os.Mkdir(filepath.Join(stage, "configs"), 0o755); err != nil {
		return fail(err)
	}
	binary := filepath.Join(stage, "aries-setup")

	if opts.NodeBinary == "" {
		utils.WaitPrintf("Building aries-setup for linux/%s", opts.NodeArch)
		root, err := utils.ExecShellCmd("go env GOMOD")
		if err != nil || root == "" || root == os.DevNull {
			return fail(fmt.Errorf("cannot locate the ARIES module to build the node binary; run from the repository or pass --node-binary"))
		}
		if _, err := utils.ExecShellCmd("cd %s && CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -o %s ./setup/aries-setup",
			utils.Quote(filepath.Dir(root)), utils.Quote(opts.NodeArch), utils.Quote(binary)); err != nil {
			return fail(err)
		}
	} else if _, err := utils.ExecShellCmd("install -m 0755 %s %s", utils.Quote(opts.NodeBinary), utils.Quote(binary)); err != nil {
		return fail(err)
	}

	shipped := []string{"kube.json", "system.json"}
	if opts.Cluster.DeployPrometheus {
		if err := os.Mkdir(filepath.Join(stage, "configs", configs.PrometheusDir), 0o755); err != nil {
			return fail(err)
		}
		shipped = append(shipped, filepath.Join(configs.PrometheusDir, "prom_config.json"))
	}
	if opts.Cluster.DeployAries {
		if err := os.Mkdir(filepath.Join(stage, "configs", configs.AriesDir), 0o755); err != nil {
			return fail(err)
		}
		shipped = append(shipped, filepath.Join(configs.AriesDir, "aries_config.json"))
	}
	// The charts live outside the configs directory, so they are copied as
	// whole trees rather than named files. Only what is being deployed is
	// shipped: the vendored monitoring chart alone is 6.7MB.
	if opts.Cluster.DeployPrometheus || opts.Cluster.DeployAries {
		if err := stageCharts(stage, opts); err != nil {
			return fail(err)
		}
	}
	for _, name := range shipped {
		source := filepath.Join(opts.ConfigsDir, name)
		if _, err := os.Stat(source); os.IsNotExist(err) && name == "system.json" {
			continue // optional; every field has a default
		}
		if _, err := utils.ExecShellCmd("install -m 0644 %s %s", utils.Quote(source), utils.Quote(filepath.Join(stage, "configs", name))); err != nil {
			return fail(err)
		}
	}
	return stage, nil
}

// stageCharts copies the Helm charts being deployed into the stage, under the
// "charts" name the on-node subcommands expect.
//
// secret.yaml is copied with the rest of the ARIES chart. It holds the model
// API key, so the staged tree is 0700 and cp -p preserves the source file's
// own mode rather than widening it.
func stageCharts(stage string, opts CreateOptions) error {
	dest := filepath.Join(stage, remoteChartsDir)
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	source := configs.Charts{Dir: opts.ChartsDir}
	if opts.Cluster.DeployPrometheus {
		if err := source.RequirePrometheus(); err != nil {
			return err
		}
		for _, dir := range []string{"prometheus", "grafana"} {
			if _, err := utils.ExecShellCmd("cp -Rp %s %s",
				utils.Quote(filepath.Join(opts.ChartsDir, dir)), utils.Quote(dest)); err != nil {
				return err
			}
		}
	}
	if opts.Cluster.DeployAries {
		if err := source.RequireAries(opts.AriesValuesFiles); err != nil {
			return fmt.Errorf("%w; copy k8s/aries/secret.yaml.example to secret.yaml and fill it in", err)
		}
		if _, err := utils.ExecShellCmd("cp -Rp %s %s",
			utils.Quote(source.AriesChart()), utils.Quote(dest)); err != nil {
			return err
		}
	}
	return nil
}

// freshJoinCommand mints a new 24h token rather than reusing the saved one,
// so re-running against an existing control plane still works.
func freshJoinCommand(ssh sshClient, master string) (JoinCommand, error) {
	raw, err := ssh.run(master, "sudo kubeadm token create --print-join-command")
	if err != nil {
		return JoinCommand{}, err
	}
	join, err := ParseJoinCommand(lastLine(raw))
	if err != nil {
		return JoinCommand{}, fmt.Errorf("master did not return a usable join command: %w", err)
	}
	return join, nil
}

// joinWorkers runs every worker concurrently. Each keeps its own captured
// output so failures can be reported per node, and all failures are collected
// rather than aborting on the first.
func joinWorkers(ssh sshClient, stage string, workers []string, join JoinCommand) error {
	if len(workers) == 0 {
		utils.WarnPrintf("no worker nodes configured")
		return nil
	}
	utils.WaitPrintf("Joining %d worker(s) in parallel", len(workers))

	errs := make([]error, len(workers))
	var wg sync.WaitGroup
	for i, node := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			utils.InfoPrintf("started %s", node)
			if err := ssh.stage(node, stage); err != nil {
				errs[i] = err
				return
			}
			if _, err := ssh.run(node, onNode("setup_node")); err != nil {
				errs[i] = fmt.Errorf("setup_node: %w", err)
				return
			}
			if _, err := ssh.run(node, onNode("setup_worker", "--join", join.String())); err != nil {
				errs[i] = fmt.Errorf("setup_worker: %w", err)
				return
			}
			utils.SuccessPrintf("joined %s", node)
		}()
	}
	wg.Wait()

	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			utils.ErrorPrintf("FAILED %s: %v", workers[i], err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d worker(s) failed to join; see aries-setup-*.log in ~/%s on each", failed, remoteDir)
	}
	return nil
}

// applyRoles labels and taints each role pool. The label is what pods select
// with nodeSelector; the taint keeps everything else away. Both use
// --overwrite, so re-running is idempotent.
func applyRoles(ssh sshClient, cluster configs.Cluster) error {
	if cluster.SkipRoleTaints {
		utils.InfoPrintf("skip_role_taints: every node joined as a plain worker")
		return nil
	}
	utils.WaitPrintf("Labelling and tainting nodes by role")
	kubectl := func(args ...string) error {
		_, err := ssh.run(cluster.Master, "sudo "+kubectlAdmin+" "+utils.QuoteAll(args...))
		return err
	}

	masterName, err := nodeName(ssh, cluster.Master)
	if err != nil {
		return err
	}
	if err := kubectl("label", "node", masterName, RoleLabel+"=master", "--overwrite"); err != nil {
		return err
	}
	utils.InfoPrintf("%-8s %s", "master", masterName)

	for _, assignment := range Assignments(cluster) {
		name, err := nodeName(ssh, assignment.Target)
		if err != nil {
			return err
		}
		if err := kubectl("label", "node", name, RoleLabel+"="+assignment.Role, "--overwrite"); err != nil {
			return err
		}
		if err := kubectl("taint", "node", name, RoleLabel+"="+assignment.Role+":NoSchedule", "--overwrite"); err != nil {
			return err
		}
		// Cosmetic: `kubectl get nodes` builds its ROLES column only from
		// node-role.kubernetes.io/* labels. Scheduling still keys off RoleLabel.
		if err := kubectl("label", "node", name, "node-role.kubernetes.io/"+assignment.Role+"=", "--overwrite"); err != nil {
			return err
		}
		utils.InfoPrintf("%-8s %s", assignment.Role, name)
	}
	return nil
}

// nodeName asks a node what the kubelet registered it as. The SSH target is
// not usable: on CloudLab a host is reached as clnodeNNN.<site> but joins as
// nodeN.<experiment>.<project>.<site>.
func nodeName(ssh sshClient, target string) (string, error) {
	name, err := ssh.run(target, "hostname")
	if err != nil {
		return "", err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", fmt.Errorf("could not resolve the Kubernetes node name for %s", target)
	}
	return name, nil
}

// fetchKubeconfig copies the admin kubeconfig back. It embeds a cluster-admin
// client certificate, so the file is created 0600 before any byte is written
// and the content never passes through the command log.
func fetchKubeconfig(ssh sshClient, master, out string) error {
	utils.WaitPrintf("Fetching kubeconfig to %s", out)
	content, err := ssh.runSecret(master, "sudo cat "+adminConf)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content + "\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	utils.InfoPrintf("export KUBECONFIG=%s", out)
	utils.InfoPrintf("the server address is the master's advertise address; it must be routable from here")
	return nil
}
