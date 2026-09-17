package cluster

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

const (
	testToken = "abcdef.0123456789abcdef"
	testHash  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestParseJoinCommandAsKubeadmPrintsIt(t *testing.T) {
	line := "kubeadm join 10.10.1.1:6443 --token " + testToken + " --discovery-token-ca-cert-hash " + testHash + " \n"
	join, err := ParseJoinCommand(line)
	if err != nil {
		t.Fatal(err)
	}
	if join.Endpoint != "10.10.1.1:6443" || join.Token != testToken || join.CAHash != testHash {
		t.Errorf("join = %+v", join)
	}
	if join.String() != strings.TrimSpace(line) {
		t.Errorf("String() = %q, want the original line back", join.String())
	}
}

func TestParseJoinCommandToleratesSudoAndSocket(t *testing.T) {
	line := "sudo kubeadm join master.example:6443 --token " + testToken +
		" --discovery-token-ca-cert-hash " + testHash + " --cri-socket unix:///run/containerd/containerd.sock"
	if _, err := ParseJoinCommand(line); err != nil {
		t.Errorf("rejected a valid variant: %v", err)
	}
}

// setup_worker runs the join through a shell. Parsing and re-rendering means
// only kubeadm-shaped fields ever reach that line, however the join string was
// produced or passed along.
func TestParseJoinCommandRejectsAnythingElse(t *testing.T) {
	cases := map[string]string{
		"injected token": "kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef;rm --discovery-token-ca-cert-hash " + testHash,
		"injected host":  "kubeadm join $(id):6443 --token " + testToken + " --discovery-token-ca-cert-hash " + testHash,
		"extra flag":     "kubeadm join 10.0.0.1:6443 --token " + testToken + " --discovery-token-ca-cert-hash " + testHash + " --control-plane x",
		"dangling flag":  "kubeadm join 10.0.0.1:6443 --token",
		"short hash":     "kubeadm join 10.0.0.1:6443 --token " + testToken + " --discovery-token-ca-cert-hash sha256:abc",
		"not a join":     "kubeadm init",
		"no port":        "kubeadm join 10.0.0.1 --token " + testToken + " --discovery-token-ca-cert-hash " + testHash,
	}
	for name, line := range cases {
		if _, err := ParseJoinCommand(line); err == nil {
			t.Errorf("%s: %q should be rejected", name, line)
		}
	}
}

func testCluster() configs.Cluster {
	return configs.Cluster{
		Master:       "u@master",
		AriesNodes:   []string{"u@aries"},
		HarnessNodes: []string{"u@harness", "u@master"},
		SandboxNodes: []string{"u@sandbox", "u@harness"},
		Workers:      []string{"u@plain", "u@aries"},
	}
}

// Listing the master in a pool is harmless, and a node in several pools is
// installed exactly once.
func TestWorkersDeduplicateAndExcludeMaster(t *testing.T) {
	got := Workers(testCluster())
	want := []string{"u@aries", "u@harness", "u@sandbox", "u@plain"}
	if !slices.Equal(got, want) {
		t.Errorf("Workers = %v, want %v", got, want)
	}
}

// A node can hold one aries.dev/role; the last pool that lists it wins.
func TestRoleOfLastPoolWins(t *testing.T) {
	cluster := testCluster()
	for target, want := range map[string]string{
		"u@harness": "sandbox", "u@aries": "aries", "u@plain": "worker",
	} {
		if got := RoleOf(cluster, target); got != want {
			t.Errorf("RoleOf(%s) = %s, want %s", target, got, want)
		}
	}
}

func TestAssignmentsSkipPlainWorkersAndMaster(t *testing.T) {
	got := Assignments(testCluster())
	want := []Assignment{{"u@aries", "aries"}, {"u@harness", "sandbox"}, {"u@sandbox", "sandbox"}}
	if !slices.Equal(got, want) {
		t.Errorf("Assignments = %v, want %v", got, want)
	}
}

// runLocally replaces ssh with a function that prints its final argument, so
// the test sees exactly what the remote shell would be handed.
func runLocally(t *testing.T, line string) string {
	t.Helper()
	script := `ssh() { printf '%s' "${@: -1}"; }; ` + line
	out, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("line did not parse: %v\n%s", err, line)
	}
	return string(out)
}

// The remote command crosses two shells. After the local one, ssh must hold
// the remote command byte-for-byte.
func TestSSHLineDeliversTheRemoteCommandIntact(t *testing.T) {
	ssh := newSSH(configs.Cluster{SSHKey: "/keys/my key", SSHOptions: []string{"-o", "ConnectTimeout=15"}})
	remote := onNode("setup_worker", "--join", "kubeadm join 10.0.0.1:6443 --token "+testToken+" --discovery-token-ca-cert-hash "+testHash)
	if got := runLocally(t, ssh.Line("u@host", remote)); got != remote {
		t.Errorf("remote received %q\nwant          %q", got, remote)
	}
}

// And the remote shell must see the join command as one argument to --join.
func TestOnNodePassesJoinAsOneArgument(t *testing.T) {
	join := "kubeadm join 10.0.0.1:6443 --token " + testToken + " --discovery-token-ca-cert-hash " + testHash
	remote := onNode("setup_worker", "--join", join)
	stub := strings.Replace(remote, "cd .aries-setup && sudo ./aries-setup", `args() { printf '%s\n' "$@"; }; args`, 1)
	out, err := exec.Command("bash", "-c", stub).Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	want := []string{"--configs-dir", "configs", "--charts-dir", "charts", "setup_worker", "--join", join}
	if !slices.Equal(lines, want) {
		t.Errorf("argv = %q, want %q", lines, want)
	}
}

func TestStageLineShipsArchiveToTheRightHost(t *testing.T) {
	line := newSSH(configs.Cluster{}).StageLine("u@host", "/tmp/stage dir")
	// Assert the ordering and the quoted source directory, not the exact flag
	// list, so adding a tar option does not fail this test.
	archive, pipe := strings.Index(line, "tar -C '/tmp/stage dir'"), strings.Index(line, "| ssh ")
	if archive < 0 || pipe < 0 || archive > pipe {
		t.Errorf("stage line = %q; tar must read the staged dir and feed ssh, not the other way round", line)
	}
	if !strings.Contains(line, "-cf - .") {
		t.Errorf("stage line = %q; tar must write the archive to stdout", line)
	}
	if strings.Contains(line, "ssh -n") {
		t.Error("the staging ssh reads the archive from stdin and must not use -n")
	}
}

func TestGoArch(t *testing.T) {
	for machine, want := range map[string]string{"x86_64\n": "amd64", "aarch64": "arm64", "riscv64": "riscv64"} {
		if got := GoArch(machine); got != want {
			t.Errorf("GoArch(%q) = %q, want %q", machine, got, want)
		}
	}
}

func TestCalicoInstallationUsesThePodCIDR(t *testing.T) {
	manifest := CalicoInstallation("10.50.0.0/16")
	if !strings.Contains(manifest, "cidr: 10.50.0.0/16") || !strings.HasPrefix(manifest, "apiVersion:") {
		t.Errorf("manifest =\n%s", manifest)
	}
}

// A macOS operator's filesystem metadata must not reach a node. Every file on
// macOS carries extended attributes, and bsdtar writes each as a companion
// "._<file>" member; extracted by GNU tar on the node they become real files,
// and helm then parses one of them as a chart CRD and dies on "control
// characters are not allowed".
func TestStageLineKeepsAppleDoubleFilesOutOfTheArchive(t *testing.T) {
	line := newSSH(configs.Cluster{}).StageLine("u@host", "/tmp/stage")
	if !strings.HasPrefix(line, "COPYFILE_DISABLE=1 tar ") {
		t.Errorf("stage line must disable AppleDouble generation, got %q", line)
	}
	for _, pattern := range []string{"._*", ".DS_Store"} {
		if !strings.Contains(line, "--exclude "+utils.Quote(pattern)) {
			t.Errorf("stage line does not exclude %s: %q", pattern, line)
		}
	}
}

// --skip-workers must not touch the workers at all. Re-joining a node that
// already belongs to a cluster is refused by setup_worker, so without this a
// re-deploy of the charts on a live cluster cannot get past the join step.
func TestSkipWorkersLeavesWorkersAlone(t *testing.T) {
	topology := configs.Cluster{
		Master:       "u@master",
		AriesNodes:   []string{"u@a1"},
		HarnessNodes: []string{"u@h1"},
	}
	if got := len(Workers(topology)); got != 2 {
		t.Fatalf("fixture has %d workers, want 2", got)
	}
	// CreateCluster's first action is preflight, which fails here because the
	// hosts do not resolve. What matters is which hosts it names: with
	// --skip-workers it must stop at the master and never mention a worker.
	err := CreateCluster(CreateOptions{
		ConfigsDir: t.TempDir(), ChartsDir: t.TempDir(),
		Cluster: topology, NodeArch: "amd64",
		SkipMaster: true, SkipWorkers: true, CheckOnly: true,
	})
	if err == nil {
		t.Fatal("preflight against unresolvable hosts should fail")
	}
	for _, worker := range []string{"u@a1", "u@h1"} {
		if strings.Contains(err.Error(), worker) {
			t.Errorf("--skip-workers still contacted %s: %v", worker, err)
		}
	}
	if !strings.Contains(err.Error(), "u@master") {
		t.Errorf("the master should still be preflighted, got: %v", err)
	}
}
