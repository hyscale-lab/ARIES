package cluster

import (
	"fmt"
	"os"
	"strings"

	"github.com/hyscale-lab/aries/k8s/setup/internal/configs"
	"github.com/hyscale-lab/aries/k8s/setup/internal/utils"
)

// SetupAries installs the ARIES chart from the control plane.
//
// It runs where setup_prometheus runs, and for the same reason: the admin
// kubeconfig and the vendored charts are already there, so the operator never
// has to log in to the master or hold a kubeconfig locally to deploy.
//
// The chart pulls the public bridge image, so by default it needs nothing this
// step cannot supply. A private registry adds a secret.yaml to the values
// files; every listed file is checked before Helm runs.
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

	helm := "KUBECONFIG=" + adminConf + " " + helmBinary
	args := make([]string, 0, len(aries.ValuesFiles)*2)
	for _, name := range aries.ValuesFiles {
		args = append(args, "-f", charts.AriesFile(name))
	}

	utils.WaitPrintf("Installing the ARIES chart as %s/%s", aries.Namespace, aries.Release)
	// Values files are applied in order, so a secret.yaml goes last in
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
	utils.SuccessPrintf("the ARIES chart is deployed")
	for _, line := range AriesInstructions(aries) {
		utils.InfoPrintf("%s", line)
	}
	return nil
}

// AriesInstructions says what comes next. The chart holds only the in-cluster
// half; the runner still has to be set up on its own host.
func AriesInstructions(aries configs.Aries) []string {
	ns := utils.Quote(aries.Namespace)
	return []string{
		"the tool bridge is up; the runner runs outside the cluster:",
		"  aries-setup setup_runner       (from your machine; create_cluster runs it when cluster.json names a runner)",
		"check the bridge pod with:",
		fmt.Sprintf("  kubectl -n %s get pods -l app.kubernetes.io/name=aries-bridge", ns),
	}
}
