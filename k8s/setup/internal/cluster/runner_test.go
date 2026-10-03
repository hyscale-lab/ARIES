package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/k8s/setup/internal/configs"
)

// The runner's kubeconfig must authenticate as the ServiceAccount and default
// to the ARIES namespace; it must carry no client certificate, which is what
// admin.conf would give it.
func TestRunnerKubeconfigUsesTheServiceAccountToken(t *testing.T) {
	raw, err := RunnerKubeconfig("https://10.10.1.1:6443", "Q0EtREFUQQ==", "eyJ.token.sig", "aries")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Clusters []struct {
			Cluster map[string]string `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User map[string]string `json:"user"`
		} `json:"users"`
		Contexts []struct {
			Context map[string]string `json:"context"`
		} `json:"contexts"`
		Current string `json:"current-context"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("kubeconfig is not valid JSON: %v", err)
	}
	if len(config.Clusters) != 1 || config.Clusters[0].Cluster["server"] != "https://10.10.1.1:6443" ||
		config.Clusters[0].Cluster["certificate-authority-data"] != "Q0EtREFUQQ==" {
		t.Errorf("cluster = %+v", config.Clusters)
	}
	if len(config.Users) != 1 || config.Users[0].User["token"] != "eyJ.token.sig" || len(config.Users[0].User) != 1 {
		t.Errorf("user must hold only the token: %+v", config.Users)
	}
	if config.Current == "" || len(config.Contexts) != 1 || config.Contexts[0].Context["namespace"] != "aries" {
		t.Errorf("context = %+v current %q", config.Contexts, config.Current)
	}
	if _, err := RunnerKubeconfig("https://x:6443", "", "t", "aries"); err == nil {
		t.Error("a kubeconfig without a CA must be refused, not written insecure")
	}
}

// kubectl is matched to the server version and checked against its published
// digest before it is installed.
func TestInstallRunnerToolsVerifiesKubectl(t *testing.T) {
	script := installRunnerTools("v1.34.2", "arm64")
	for _, want := range []string{
		"set -e",
		"https://dl.k8s.io/release/v1.34.2/bin/linux/arm64/kubectl",
		"kubectl.sha256",
		"sha256sum -c -",
		"install -m 0755",
		"git",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script lacks %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "sha256sum -c -") > strings.Index(script, "install -m 0755") {
		t.Error("kubectl is installed before its digest is checked")
	}
}

// What reaches the runner host: the binaries, profiles and configs, and
// nothing that belongs to the operator's machine or holds a credential.
func TestRunnerStageShipsTheRunnerAndNoSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the runner for linux")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	stage, err := buildRunnerStage(root, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	var top []string
	entries, err := os.ReadDir(stage)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		top = append(top, entry.Name())
	}
	if !slices.Equal(top, []string{"bin", "configs", "profiles"}) {
		t.Errorf("stage holds %v, want exactly bin, configs, profiles", top)
	}
	for _, binary := range []string{"aries", "aries-ssh"} {
		info, err := os.Stat(filepath.Join(stage, "bin", binary))
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Errorf("bin/%s missing or not executable: %v", binary, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stage, "configs", "versions.json")); err != nil {
		t.Errorf("profiles resolve ../configs/versions.json, which is missing: %v", err)
	}
	_ = filepath.WalkDir(stage, func(path string, entry os.DirEntry, err error) error {
		if err == nil && (strings.HasSuffix(entry.Name(), ".key") || entry.Name() == "secret.yaml" || entry.Name() == "cluster.json") {
			t.Errorf("stage carries a credential or topology file: %s", path)
		}
		return nil
	})
}

// Re-staging replaces only what setup_runner owns. runs/, .cache/ and the
// model key under the runner directory must survive it.
func TestRunnerStageLineOnlyReplacesStagedDirectories(t *testing.T) {
	ssh := newSSH(configs.Cluster{Master: "u@m"})
	remote := "mkdir -p 'aries' && cd 'aries' && rm -rf " + strings.Join(runnerStaged, " ") + " && tar -xf -"
	line := ssh.stageLine("u@runner", "/tmp/stage", remote)
	for _, kept := range []string{"runs", ".cache", "DEEPSEEK_API.key", "kubeconfig"} {
		if slices.Contains(runnerStaged, kept) {
			t.Errorf("setup_runner would delete %s", kept)
		}
	}
	if !strings.Contains(line, "COPYFILE_DISABLE=1") || !strings.Contains(line, "--exclude") {
		t.Errorf("runner stage line lost the AppleDouble guards: %s", line)
	}
}

// The chart directory also holds gitignored local files that may carry
// credentials. Only the values files Helm is given may reach the master;
// templates, including templates/secrets.yaml, are untouched.
func TestStageChartsShipsOnlyTheValuesFilesHelmIsGiven(t *testing.T) {
	charts := t.TempDir()
	chart := filepath.Join(charts, "aries")
	for name, content := range map[string]string{
		"Chart.yaml": "name: aries\n", "values.yaml": "a: 1\n", "values-cluster.yaml": "b: 1\n",
		"secret.yaml": "model:\n  apiKey: sk-must-not-ship\n", "values-local-cloudlab.yaml": "c: 1\n",
		"secret.yaml.example": "x\n", "templates/secrets.yaml": "kind: Secret\n", "dashboards/a.json": "{}",
	} {
		path := filepath.Join(chart, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stage := t.TempDir()
	err := stageCharts(stage, CreateOptions{ChartsDir: charts, AriesValuesFiles: []string{"values-cluster.yaml"},
		Cluster: configs.Cluster{DeployAries: true}})
	if err != nil {
		t.Fatal(err)
	}
	var staged []string
	_ = filepath.WalkDir(filepath.Join(stage, remoteChartsDir, "aries"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(stage, remoteChartsDir, "aries"), path)
			staged = append(staged, filepath.ToSlash(rel))
		}
		return err
	})
	slices.Sort(staged)
	want := []string{"Chart.yaml", "dashboards/a.json", "secret.yaml.example", "templates/secrets.yaml", "values-cluster.yaml", "values.yaml"}
	if !slices.Equal(staged, want) {
		t.Errorf("staged %v, want %v", staged, want)
	}
}
