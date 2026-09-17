package cluster

import (
	"fmt"
	"os"
	"strings"

	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

// SetupAries installs the ARIES chart from the control plane.
//
// It runs where setup_prometheus runs, and for the same reason: the admin
// kubeconfig and the vendored charts are already there, so the operator never
// has to log in to the master or hold a kubeconfig locally to deploy.
//
// The chart needs two things this step cannot supply: an image the cluster can
// pull, and a filled-in secret.yaml among the values files. Both are checked
// before Helm runs.
func SetupAries(aries configs.Aries, charts configs.Charts) error {
	if err := utils.RequireRoot(); err != nil {
		return err
	}
	if _, err := os.Stat(adminConf); err != nil {
		return fmt.Errorf("%s not found; setup_aries runs on an initialised control plane", adminConf)
	}
	if err := charts.RequireAries(aries.ValuesFiles); err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			return fmt.Errorf("%w\ncopy k8s/aries/secret.yaml.example to k8s/aries/secret.yaml and fill it in; create_cluster ships it here", err)
		}
		return err
	}

	// Fail now rather than leave the pod Pending forever: the chart pins ARIES
	// to the aries role pool, and every role node is tainted, so nothing lands
	// on an unlabelled node.
	utils.WaitPrintf("Checking for nodes labelled %s=%s", RoleLabel, RoleAries)
	nodes, err := utils.ExecShellCmd(kubectlAdmin+" get nodes -l %s -o name", utils.Quote(RoleLabel+"="+RoleAries))
	if err != nil {
		return err
	}
	if strings.TrimSpace(nodes) == "" {
		return fmt.Errorf("no node is labelled %s=%s; label the role pools first (create_cluster does)", RoleLabel, RoleAries)
	}
	for _, node := range strings.Fields(nodes) {
		utils.InfoPrintf("ARIES runs on %s", strings.TrimPrefix(node, "node/"))
	}

	helm := "KUBECONFIG=" + adminConf + " " + helmBinary
	args := make([]string, 0, len(aries.ValuesFiles)*2)
	for _, name := range aries.ValuesFiles {
		args = append(args, "-f", charts.AriesFile(name))
	}

	utils.WaitPrintf("Installing the ARIES chart as %s/%s", aries.Namespace, aries.Release)
	// Values files are applied in order, so secret.yaml goes last in
	// aries_config.json and wins. The command line carries only paths: the key
	// itself is never an argument, so it does not reach the process table or
	// the command log.
	if err := utils.ExecShellCmdStreaming(helm+" upgrade --install %s %s --namespace %s --create-namespace %s --wait --timeout %s",
		utils.Quote(aries.Release), utils.Quote(charts.AriesChart()), utils.Quote(aries.Namespace),
		utils.QuoteAll(args...), utils.Quote(aries.Timeout)); err != nil {
		return err
	}

	pods, _ := utils.ExecShellCmd(kubectlAdmin+" get pods -n %s -o wide", utils.Quote(aries.Namespace))
	utils.InfoPrintf("\n%s", pods)
	utils.SuccessPrintf("ARIES is deployed")
	for _, line := range AriesInstructions(aries) {
		utils.InfoPrintf("%s", line)
	}
	return nil
}

// AriesInstructions tells the operator how to start a run. The pod idles, so
// nothing happens until they do.
func AriesInstructions(aries configs.Aries) []string {
	ns := utils.Quote(aries.Namespace)
	return []string{
		"the pod idles; ARIES is a batch runner, so trigger a run yourself:",
		fmt.Sprintf("  kubectl -n %s exec deploy/aries -- sh -c './bin/aries \"$ARIES_PROFILE\"'", ns),
		"then collect the artifacts:",
		fmt.Sprintf("  kubectl -n %s cp <pod>:/app/runs ./runs", ns),
	}
}
