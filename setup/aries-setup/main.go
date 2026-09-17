// Command aries-setup builds the Kubernetes cluster ARIES runs on.
//
// On-node subcommands run on the machine being set up, as root:
//
//	setup_node          install containerd and kubelet/kubeadm/kubectl (every node)
//	setup_master_node   kubeadm init, CNI, write a join command    (control plane)
//	setup_worker        join the control plane                      (each worker)
//	reset_node          tear the node back to a pre-kubeadm state   (any node)
//
//	setup_prometheus    install Prometheus + Grafana                 (control plane)
//	setup_aries         install the ARIES chart                      (control plane)
//
// create_cluster runs on the operator's machine and drives those same
// subcommands on every node over SSH, then labels and taints the role pools.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/hyscale-lab/aries/setup/internal/cluster"
	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/node"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

const usageText = `Usage: aries-setup [--configs-dir DIR] <subcommand> [flags]

On a node (as root):
  setup_node                         install containerd and the kube packages
  setup_master_node                  initialise the control plane
  setup_worker --join "<cmd>"        join this node to the control plane
  setup_prometheus                   install Prometheus + Grafana (control plane)
  setup_aries                        install the ARIES chart (control plane)
  reset_node --yes                   destroy this node's cluster state

From your machine:
  create_cluster [--check] [--reset] [--skip-master] [--skip-workers]
                 [--node-binary PATH] [--node-arch ARCH]

  Re-deploy the charts on a cluster that already exists:
  create_cluster --skip-master --skip-workers

Global flags:
`

var subcommands = []string{"setup_node", "setup_master_node", "setup_worker", "setup_prometheus", "setup_aries", "reset_node", "create_cluster"}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	global := flag.NewFlagSet("aries-setup", flag.ContinueOnError)
	configsDir := global.String("configs-dir", filepath.Join("setup", "configs"),
		"directory holding kube.json, system.json and (for create_cluster) cluster.json")
	chartsDir := global.String("charts-dir", "k8s",
		"directory holding the aries, prometheus and grafana Helm charts")
	global.Usage = func() {
		fmt.Fprint(os.Stderr, usageText)
		global.PrintDefaults()
	}
	if err := global.Parse(args); err != nil {
		return usageExit(err)
	}
	if global.NArg() < 1 {
		global.Usage()
		return 2
	}
	sub, subArgs := global.Arg(0), global.Args()[1:]
	// Validate before the name is used to build the log file path, so an
	// unknown or path-like argument never creates a file.
	if !slices.Contains(subcommands, sub) {
		utils.ErrorPrintf("unknown subcommand %q (try --help)", sub)
		return 2
	}

	if err := utils.OpenLog(fmt.Sprintf("aries-setup-%s.log", sub)); err != nil {
		utils.ErrorPrintf("%v", err)
		return 1
	}
	defer utils.CloseLog()

	if err := dispatch(sub, subArgs, *configsDir, *chartsDir); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		utils.ErrorPrintf("%s failed: %v", sub, err)
		return 1
	}
	return 0
}

func usageExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func dispatch(sub string, args []string, configsDir, chartsDir string) error {
	charts := configs.Charts{Dir: chartsDir}
	switch sub {
	case "setup_node":
		if err := parseNone(sub, args); err != nil {
			return err
		}
		kube, system, err := loadNodeConfigs(configsDir)
		if err != nil {
			return err
		}
		return node.SetupNode(kube, system)

	case "setup_master_node":
		if err := parseNone(sub, args); err != nil {
			return err
		}
		kube, system, err := loadNodeConfigs(configsDir)
		if err != nil {
			return err
		}
		return cluster.SetupMasterNode(kube, system)

	case "setup_worker":
		flags := flag.NewFlagSet(sub, flag.ContinueOnError)
		joinLine := flags.String("join", "", `full "kubeadm join ..." command printed by setup_master_node`)
		endpoint := flags.String("api-server", "", "control-plane host:port (instead of --join)")
		token := flags.String("token", "", "bootstrap token (instead of --join)")
		hash := flags.String("discovery-hash", "", "sha256:<hash> (instead of --join)")
		if err := flags.Parse(args); err != nil {
			return err
		}
		join, err := joinFromFlags(*joinLine, *endpoint, *token, *hash)
		if err != nil {
			return err
		}
		kube, system, err := loadNodeConfigs(configsDir)
		if err != nil {
			return err
		}
		return cluster.SetupWorker(join, kube, system)

	case "setup_prometheus":
		if err := parseNone(sub, args); err != nil {
			return err
		}
		if runtime.GOOS != "linux" {
			return fmt.Errorf("setup_prometheus runs on the control plane, not on %s", runtime.GOOS)
		}
		prom, err := configs.LoadPrometheus(configsDir)
		if err != nil {
			return err
		}
		return cluster.SetupPrometheus(prom, charts)

	case "setup_aries":
		if err := parseNone(sub, args); err != nil {
			return err
		}
		if runtime.GOOS != "linux" {
			return fmt.Errorf("setup_aries runs on the control plane, not on %s", runtime.GOOS)
		}
		aries, err := configs.LoadAries(configsDir)
		if err != nil {
			return err
		}
		return cluster.SetupAries(aries, charts)

	case "reset_node":
		flags := flag.NewFlagSet(sub, flag.ContinueOnError)
		confirmed := flags.Bool("yes", false, "confirm destroying all cluster state on this node")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if !*confirmed {
			hostname, _ := os.Hostname()
			return fmt.Errorf("refusing to reset without --yes; this destroys all cluster state on %s", hostname)
		}
		system, err := configs.LoadSystem(configsDir)
		if err != nil {
			return err
		}
		return node.ResetNode(system)

	case "create_cluster":
		flags := flag.NewFlagSet(sub, flag.ContinueOnError)
		check := flags.Bool("check", false, "verify SSH, passwordless sudo and architecture on every node, then stop")
		reset := flags.Bool("reset", false, "reset every node first; DESTROYS all cluster state, including hostPath volume contents")
		skipMaster := flags.Bool("skip-master", false, "only join workers against an already-initialised control plane")
		skipWorkers := flags.Bool("skip-workers", false, "leave the existing workers alone; with --skip-master this runs only the role labels and the chart deployments")
		nodeBinary := flags.String("node-binary", "", "prebuilt linux aries-setup to ship; empty builds one from this repository")
		nodeArch := flags.String("node-arch", "amd64", "GOARCH of the nodes")
		kubeconfigOut := flags.String("kubeconfig-out", "", "where fetch_kubeconfig writes; default <configs-dir>/../kubeconfig")
		if err := flags.Parse(args); err != nil {
			return err
		}
		topology, err := configs.LoadCluster(configsDir)
		if err != nil {
			return err
		}
		// Validate what will be shipped before touching any node.
		if _, err := configs.LoadKube(configsDir); err != nil {
			return err
		}
		if _, err := configs.LoadSystem(configsDir); err != nil {
			return err
		}
		if topology.DeployPrometheus {
			prom, err := configs.LoadPrometheus(configsDir)
			if err != nil {
				return err
			}
			if err := cluster.CheckPrometheusPlacement(topology, prom); err != nil {
				return err
			}
			if err := charts.RequirePrometheus(); err != nil {
				return err
			}
		}
		// Validated here, before any node is touched: a missing secret.yaml or
		// a malformed values list should fail in a second, not after the whole
		// cluster is built.
		var ariesValues []string
		if topology.DeployAries {
			aries, err := configs.LoadAries(configsDir)
			if err != nil {
				return err
			}
			if err := charts.RequireAries(aries.ValuesFiles); err != nil {
				return fmt.Errorf("%w\ncopy k8s/aries/secret.yaml.example to k8s/aries/secret.yaml and fill it in", err)
			}
			if len(topology.AriesNodes) == 0 {
				return fmt.Errorf("deploy_aries needs at least one entry in aries_nodes; the chart pins ARIES to that role pool")
			}
			ariesValues = aries.ValuesFiles
		}
		// --skip-master --skip-workers with nothing to deploy would preflight the
		// master, build a node binary and then do nothing at all.
		if *skipMaster && *skipWorkers && !topology.DeployPrometheus && !topology.DeployAries {
			return fmt.Errorf("--skip-master --skip-workers leaves nothing to do; set deploy_prometheus or deploy_aries in cluster.json, or drop a --skip flag")
		}
		if *skipWorkers && *reset {
			return fmt.Errorf("--reset resets every node, which contradicts --skip-workers; drop one")
		}

		out := *kubeconfigOut
		if out == "" {
			out = filepath.Join(filepath.Dir(filepath.Clean(configsDir)), "kubeconfig")
		}
		return cluster.CreateCluster(cluster.CreateOptions{
			ConfigsDir: configsDir, ChartsDir: chartsDir, AriesValuesFiles: ariesValues,
			Cluster:    topology,
			NodeBinary: *nodeBinary, NodeArch: *nodeArch, KubeconfigOut: out,
			CheckOnly: *check, Reset: *reset,
			SkipMaster: *skipMaster, SkipWorkers: *skipWorkers,
		})
	}
	return fmt.Errorf("unknown subcommand %q (try --help)", sub)
}

func parseNone(sub string, args []string) error {
	flags := flag.NewFlagSet(sub, flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%s takes no arguments; settings come from kube.json", sub)
	}
	return nil
}

func loadNodeConfigs(dir string) (configs.Kube, configs.System, error) {
	if runtime.GOOS != "linux" {
		return configs.Kube{}, configs.System{}, fmt.Errorf("on-node subcommands run on the Linux node, not on %s; use create_cluster from here", runtime.GOOS)
	}
	kube, err := configs.LoadKube(dir)
	if err != nil {
		return configs.Kube{}, configs.System{}, err
	}
	system, err := configs.LoadSystem(dir)
	return kube, system, err
}

func joinFromFlags(line, endpoint, token, hash string) (cluster.JoinCommand, error) {
	if line != "" {
		return cluster.ParseJoinCommand(line)
	}
	if endpoint == "" || token == "" || hash == "" {
		return cluster.JoinCommand{}, fmt.Errorf(`need --join "<kubeadm join ...>", or all of --api-server, --token and --discovery-hash`)
	}
	join := cluster.JoinCommand{Endpoint: endpoint, Token: token, CAHash: hash}
	return join, join.Validate()
}
