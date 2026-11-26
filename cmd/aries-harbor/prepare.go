package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

const debianCurlSHA = "6e8e1dac4bc58e974f2654b53c6e4d5210cc8b7f492f1ad5660cdab5ac4c6998"
const codexPrefixSHA = "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab"

const royTask = "roy-polymorph-cn"
const royShellEnvironment = `export PATH=/opt/venv/bin:/root/.local/bin:"$PATH"; `
const royInputCheck = `export LC_ALL=C
test "$(sha256sum data/*.mol data/TB3_Conf_Properties.csv | sha256sum)" = '236aeefd45cca191035b28b385bc8226eca7b4c2bf8fb1d8966bfd7b8f288bb4  -'`
const royInitialCheck = `test -d /results
test ! -e /results/TB3_Conf_Answers.csv
test ! -e /app/fit_roy.py
if command -v apply_patch; then exit 1; fi
test "$(command -v python)" = /opt/venv/bin/python
python - <<'PY'
from importlib.metadata import version
from importlib.util import find_spec
import sys

assert sys.version_info[:2] == (3, 11)
assert find_spec("sklearn") is None
for name, expected in {"pandas": "3.0.3", "numpy": "2.4.6", "scipy": "1.17.1", "rdkit": "2026.3.2"}.items():
    actual = version(name)
    assert actual == expected, (name, actual)
    print(name, actual)
PY`

type taskSetup struct {
	packagesSHA, packagesField, output, inputCheck, initialCheck string
}

var taskSetups = map[string]taskSetup{
	"mvcc-lsm-compaction": {
		output: "src/flush_builder.cc",
		initialCheck: `export LC_ALL=C
test "$(sha256sum Makefile crash_report.txt include/mvcc_lsm/* src/* tests/* | sha256sum)" = '72c9ad7ceab66dc04c0f923ed2d4933931994a1e1e6e12934663fc7b216ebfdf  -'
command -v git patch make g++ rg python3
test ! -e .git
mkdir -p /tmp/codex-home
` + noShellPatch,
	},
	"freight-dispatch-shift": {
		output: "/workspace/dispatch",
		initialCheck: `test ! -e /workspace/dispatch
test ! -e /app/packet/events
test -z "${DISPATCH_EVENT_API_TOKEN+x}"
printf '\n127.0.0.1 event-feed\n' >> /etc/hosts
python3 -c "import urllib.request as u; assert u.urlopen('http://event-feed:8080/health', timeout=5).status == 200"`,
		inputCheck: `test "$(cd /app/packet && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum)" = 'cd80264da9db617a531055ef7c32c48bccdc0828957a15f9c48ad2d3748a2e0b  -'`,
	},
	"intrastat-meldung": intrastatSetup,
	paymentsTask:        paymentsSetup,
	claimsTask:          claimsSetup,
	"embedding-drift-monitor": {
		packagesSHA: "1409c8778df5c00c86abe5d99ca46b7526d3d7e5369d2596334ae543dc410a26", packagesField: "noble_git_packages_sha256",
		output: "drift_monitor/monitor.py",
		initialCheck: `export LC_ALL=C
test "$(sha256sum drift_monitor/*.py | sha256sum)" = 'a321e7570ddc8e9b6220dc3bd2e9c38b209eb49ab0398558e8dd521d0c27157c  -'
test "$(git --version)" = 'git version 2.43.0'
test ! -d .git
if command -v python || command -v apply_patch; then exit 1; fi
/usr/bin/python3 -c "import numpy, scipy; assert (numpy.__version__, scipy.__version__) == ('1.26.4', '1.11.4')"`,
		inputCheck: `export LC_ALL=C
test "$(sha256sum data/* | sha256sum)" = '6e411a090777800a3feede9bf824630790144ba7b3bb9d5c65d2b8ee39ef9718  -'`,
	},
	hofTopologyTask:            hofTopologyOfflineSetup,
	bipedTask:                  bipedSetup,
	pretrainShardTask:          pretrainShardSetup,
	vppLossTask:                vppLossSetup,
	soundCascadeTask:           soundCascadeSetup,
	"freecad-platform-drawing": {output: "/app/part.FCStd", inputCheck: "echo '51b9da9c17a09b25fc00b7b38f290fb729cb6d67c8b0f643d0c02fe80debe360  /app/e0fe7718e6.png' | sha256sum -c -", initialCheck: "test ! -e /app/part.FCStd && test ! -e /app/build_part.py\n" + cadReady},
	"freecad-spring-clip":      {output: "/app/answer_base.FCStd", initialCheck: "test ! -e /app/answer.py && test ! -e /app/answer_edit.FCStd && test ! -e /app/answer_base.FCStd\n" + cadReady},
	"music-harmony":            musicHarmonySetup,
	royTask:                    {output: "/results/TB3_Conf_Answers.csv", inputCheck: royInputCheck, initialCheck: royInitialCheck},
	"wal-recovery-ordering":    walRecoverySetup,
	"session-window-debug":     sessionWindowSetup,
	"cargo-flight-dispatch":    cargoFlightSetup,
	"coq-block-bound":          coqBlockSetup,
	"sanitize-git-repo":        {output: "README.md", initialCheck: "test -s README.md && test -s ray_processing/process.py && command -v git"},
	bnFitTask:                  bnFitSetup,
	"git-leak-recovery":        {packagesSHA: sparqlSetup.packagesSHA, output: "secret.txt", initialCheck: gitRecoveryReady},
	datasetTask:                datasetSetup,
	markerTask:                 markerSetup,
	financialTask:              financialSetup,
	sparqlTask:                 sparqlSetup,
	torchTask:                  torchSetup,
	"compile-compcert":         compilerSetup,
	kvTask:                     kvSetup,
	hfTask:                     hfSetup,
	"video-processing": {output: "output.toml", initialCheck: videoInitialCheck,
		inputCheck: `echo '69ac53c9b4302225e3bdf5b94970e29dade7d800ba07109c2691604b2cac69ad  example_video.mp4' | sha256sum -c -`},
	"circuit-fibsqrt": {output: "gates.txt", initialCheck: `echo '032786f94f16605767c62bb61ce4dbba7ab7d3f1046712c975f01c9a04b6ad29  gates.txt' | sha256sum -c -
test ! -e sim
gcc --version | head -1`, inputCheck: `echo '700645ac6156cdc88ec1e210f7c7d8d9969bb42aec969c410597e8efca796173  sim.c' | sha256sum -c -`},
	"code-from-image": {output: "output.txt", initialCheck: `test ! -e output.txt
command -v python
python --version`, inputCheck: `echo 'f4d0330407b363a9ef03d563e5c2ffd24aa76345f997613741f9fc0935354305  code.png' | sha256sum -c -`},
	"overfull-hbox": {packagesSHA: "a63ab01c6a16caeec664ffbc10890ce597e05948b0a378434f5c48b9266a8f9a", output: "input.tex", initialCheck: `echo '7bf7dbe58dbbfb08b6a652c547daa09a57cdbccc0fd18141b4968756061326a3  input.tex' | sha256sum -c -
test ! -e main.pdf && test ! -e main.log
pdflatex --version | head -1`, inputCheck: `sha256sum -c <<'ARIESSHA'
6c07d3203fdc3fc54317fafeee9d19b576418c9de2e142a599814b2f6cb32b2f  main.tex
bc8f7cf4b41914be036e6d4666e1550971d39b2f23b7e69e3e975f4bdb387be6  synonyms.txt
ARIESSHA`},
	"regex-log": {packagesSHA: "16323fa3c8382539929ca3829342150e9bb50f07379dfe89d6394c092f0c2f4b", output: "regex.txt"},
	"constraints-scheduling": {packagesSHA: "c5ec5579c923cc5a150ad21c262ebbf52c7f7183f6121ef452035cbad0c6d2bc", output: "meeting_scheduled.ics", inputCheck: `sha256sum -c <<'ARIESSHA'
5a5d45804bffd339c2479d49e76fe817b38244d14d389afe1de583443808ecbc  alice_calendar.ics
c97e85dbcdd756758f8a80f4db5fd5c1dcacee79298e4d2262f46de5ff680b50  bob_calendar.ics
32a0f8687637df0a12b8e5c56d49c4192bc3e790856943632d70f237c7c1363b  carol_calendar.ics
ARIESSHA`},
}

const debianPatchTrial = "99c36c5d-aba5-476b-93b2-d08a39c7a353"

// Debian Bookworm's signed Packages index identifies patch_2.7.6-7_amd64.deb.
// The circuit invokes GNU patch, and video trial 07e979df locates it in PATH.
func sourceSetup(r *harbor.Recording) (taskSetup, bool) {
	if r.Task == nextjsTask {
		return nextjsSourceSetup(r.Trial)
	}
	if r.Task == paymentsTask {
		return paymentsSourceSetup(r.Trial), true
	}
	task, ok := taskSetups[r.Task]
	if alias := intrastatAliases[r.Trial]; alias != "" {
		task.initialCheck += strings.ReplaceAll(intrastatAliasSetup, "ALIAS", alias)
	}
	if r.Trial == "3102c53f-5ea4-45e6-b7bf-2ee5c8698518" {
		// This recording invokes the original argv[0] patch alias by its full path.
		task.initialCheck += `
mkdir -p /root/.codex/tmp/arg0/codex-arg0PK2zte
ln -s /root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex /root/.codex/tmp/arg0/codex-arg0PK2zte/apply_patch
test -x /root/.codex/tmp/arg0/codex-arg0PK2zte/apply_patch`
	}
	if r.Trial == hofSystreTrial || r.Trial == hofExperimentalTrial || r.Trial == hofGavrogTrial || r.Trial == hofReleaseTrial {
		alias := "codex-arg0w7w0OR"
		if r.Trial == hofExperimentalTrial {
			alias = "codex-arg0baCVWK"
			task.packagesSHA = "0593c8378d11b09baa24247d774653cb617ea3e7b6f7d72c31dabb0c4de81663"
			task.initialCheck = strings.Replace(task.initialCheck, "'numpy',", "'ase', 'numpy',", 1)
		}
		if r.Trial == hofGavrogTrial {
			alias = "codex-arg0OtJsdz"
		}
		if r.Trial == hofReleaseTrial {
			alias = "codex-arg0vcHD7F"
		}
		task.initialCheck = strings.ReplaceAll(task.initialCheck, "codex-arg02OZDT5", alias)
		task.initialCheck += financialAptSetup(financialAptProfile{Snapshot: "20260831T073500Z"})
		if r.Trial == hofGavrogTrial {
			task.initialCheck += hofGavrogGitSetup
		}
		if r.Trial == hofReleaseTrial {
			// Prime the signed indexes so package installation rather than index transfer drives the short JRE poll.
			task.initialCheck += "\napt-get -o APT::Update::Error-Mode=any update -qq"
		}
	}
	if r.Task == "git-leak-recovery" {
		task.initialCheck += gitRecoveryTemp(r.Trial)
	}
	if r.Task == "compile-compcert" {
		task.initialCheck = compilerAPT(r.Plan.StartedAt) + task.initialCheck
	}
	if r.Task == markerTask {
		profile, bound := markerTrials[r.Trial]
		ok = bound
		task.initialCheck = markerReady(r.Trial, profile.stamp)
	}
	if r.Task == hfTask {
		_, ok = hfTrials[r.Trial]
		task.initialCheck += hfSourceSetup(r.Trial)
	}
	if profile, ok := financialAptTrials[r.Trial]; ok {
		task.initialCheck += financialAptSetup(profile)
	}
	if r.Trial == financialMuPDFTrial {
		task.initialCheck = strings.Replace(task.initialCheck, "21:28:16Z", "22:40:09Z", 1)
	}
	if r.Trial == "d3bd8729-d595-4aa1-86c6-7efbfc31d835" {
		task.initialCheck = strings.Replace(task.initialCheck, "21:28:16Z", "22:07:09Z", 1) + financialTempAllocation
	}
	if r.Trial == videoPatchTrial {
		task.initialCheck += "\n" + videoPatchAlias
	}
	if r.Trial == "427a5c05-723c-407c-bdbd-a2927203c0eb" {
		task.packagesSHA = debianCurlSHA
		task.packagesField = "debian_curl_archive_sha256"
		task.initialCheck += "\ncommand -v curl\ndpkg-query -W curl libcurl4"
	}
	if r.Trial == datasetPythonTrial {
		task.initialCheck = datasetPythonCheck + task.initialCheck
	}
	if r.Trial == videoHarnessTrial || r.Trial == datasetPatchDiscoveryTrial || r.Trial == financialHarnessTrial {
		task.initialCheck += "\n" + sourceHarnessDirectory
	}
	if r.Trial == debianPatchTrial || r.Trial == videoHarnessTrial || r.Trial == videoGNUPatchTrial || r.Trial == datasetPatchDiscoveryTrial {
		task.packagesField = "debian_patch_archive_sha256"
		task.packagesSHA = "2d1665bc3a472a5e895657f41612ecf4e40e4349757d6481a19420b57b8146d0"
		task.initialCheck += "\ntest \"$(command -v patch)\" = /usr/bin/patch\ndpkg-query -W patch"
	}
	return tb41SourceSetup(r.Trial, task), ok
}

func checked(ctx context.Context, m *vhive.Manager, id string, cmd core.Command) (string, error) {
	res, err := m.Exec(ctx, id, cmd)
	if err != nil || res.ExitCode != 0 {
		return "", errors.Join(fmt.Errorf("guest %s exited %d: %s %s", cmd.Path, res.ExitCode, res.Stdout, res.Stderr), err)
	}
	return res.Stdout, nil
}

func uploadPinned(ctx context.Context, m *vhive.Manager, id, file, expected, dir string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != expected {
		return fmt.Errorf("incorrect digest for %s", file)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer r.Close()
	return m.UploadArchive(ctx, id, dir, r)
}

func prepareTask(ctx context.Context, m *vhive.Manager, id string, r *harbor.Recording, tools, packages, curlPackages, grpcTools, hfHarness, shardSciPy string) (string, any, error) {
	if r.Task == "fix-git" {
		out, err := checked(ctx, m, id, core.Command{Path: "/usr/local/bin/python3", Args: []string{"-c", initialGitState}, Dir: r.Workdir})
		if err != nil || !json.Valid([]byte(out)) {
			return "", nil, errors.Join(errors.New("restore source Git starting state"), err)
		}
		return "source_git_ready", json.RawMessage(out), nil
	}
	task, ok := sourceSetup(r)
	if !ok {
		return "", nil, fmt.Errorf("no ready-state definition for %s", r.Task)
	}
	if r.Task == royTask {
		command := sourceShellCommand(r.Task, r.Trial, replay.Call{Login: true, Workdir: r.Workdir, Script: "set -eu\n" + task.inputCheck + "\n" + task.initialCheck})
		out, err := checked(ctx, m, id, command)
		return "source_task_ready", map[string]string{"checks": strings.TrimSpace(out)}, err
	}
	if r.Task == "compile-compcert" {
		if err := uploadPinned(ctx, m, id, packages, compilerBootstrapSHA, "/tmp"); err != nil {
			return "", nil, err
		}
	}
	if r.Task == kvTask || r.Trial == datasetPythonTrial {
		if err := uploadPinned(ctx, m, id, packages, kvPythonSHA, "/"); err != nil {
			return "", nil, err
		}
	}
	if r.Task == hfTask {
		if err := uploadPinned(ctx, m, id, packages, hfEnvironmentSHA, "/"); err != nil {
			return "", nil, err
		}
	}
	if curlPackages != "" {
		if err := installPackages(ctx, m, id, curlPackages, debianCurlSHA); err != nil {
			return "", nil, err
		}
		task.initialCheck += "\ncommand -v curl\ndpkg-query -W curl libcurl4"
	}
	if r.Task == "intrastat-meldung" {
		if err := uploadPinned(ctx, m, id, packages, task.packagesSHA, "/tmp"); err != nil {
			return "", nil, err
		}
	} else if r.Task == hofTopologyTask {
		if err := stageHOFWheels(ctx, m, id, packages, task.packagesSHA); err != nil {
			return "", nil, err
		}
	} else if task.packagesSHA != "" {
		if err := installPackages(ctx, m, id, packages, task.packagesSHA); err != nil {
			return "", nil, err
		}
	}
	if err := uploadPinned(ctx, m, id, tools, codexPrefixSHA, "/"); err != nil {
		return "", nil, err
	}
	if grpcTools != "" || r.Task == hfTask || r.Task == datasetTask || r.Task == "code-from-image" || r.Task == "circuit-fibsqrt" || r.Task == "video-processing" {
		// This Python base image has only one copy of codex-path in its recorded PATH.
		_, err := checked(ctx, m, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `set -eu
cat > /etc/profile.d/zz-aries-image-path.sh <<'ARIESPATH'
case "$PATH" in
    *codex-path:*codex-path:*) export PATH="${PATH#*:}" ;;
esac
ARIESPATH`}})
		if err != nil {
			return "", nil, err
		}
	}
	if r.Trial == "b43d6be8-70d4-44fb-97a6-358b6bb55b3f" || r.Trial == "5be2716d-f7a5-476e-8b31-718f12a6906b" {
		// This recorded ls requires the harness directory. Its transient logs
		// and credential-bearing state are deliberately absent from the fixture.
		if _, err := checked(ctx, m, id, core.Command{Path: "/bin/mkdir", Args: []string{"-m", "0700", "-p", "/tmp/codex-home"}}); err != nil {
			return "", nil, err
		}
	}
	initial := task.initialCheck
	if shardSciPy != "" {
		if _, err := checked(ctx, m, id, core.Command{Path: "/usr/bin/install", Args: []string{"-d", pretrainShardWheelDirectory}}); err != nil {
			return "", nil, err
		}
		if err := uploadPinned(ctx, m, id, shardSciPy, pretrainShardSciPyArchive, pretrainShardWheelDirectory); err != nil {
			return "", nil, err
		}
		initial += pretrainShardSciPyReady
	}
	if hfHarness != "" {
		if err := prepareHFInspection(ctx, m, id, hfHarness); err != nil {
			return "", nil, err
		}
		initial += hfInspectionReady
	}
	if grpcTools != "" {
		if err := installPackages(ctx, m, id, grpcTools, grpcSystemToolsSHA); err != nil {
			return "", nil, err
		}
		initial = "mkdir -p /var/lib/aries-replay\n" + strings.ReplaceAll(initial, "/tmp/aries-kv-install.json", grpcReportPath) + grpcToolsReady
	}
	if initial == "" {
		initial = "test ! -e " + task.output
	}
	allocation := ""
	if allocationNames, ok := recordedTempDirectories[r.Trial]; ok {
		install := strings.NewReplacer("SOURCE_TEMPLATE", allocationNames[0], "SOURCE_DIR", allocationNames[1]).Replace(installRecordedTempAllocation)
		out, err := checked(ctx, m, id, core.Command{Path: "/bin/sh", Args: []string{"-c", install}})
		if err != nil {
			return "", nil, err
		}
		allocation = strings.TrimSpace(out)
	}
	node := "node"
	toolCheck := "command -v node codex rg python3"
	if r.Task == nextjsTask && r.Trial != nextjsWave2Trial && r.Trial != nextjsWave3Trial {
		toolCheck = "command -v node codex rg\n/opt/aries-python/bin/python3 --version"
	}
	if r.Task == "freecad-platform-drawing" || r.Task == "freecad-spring-clip" {
		// The task prepends its Conda Node installation in login shells.
		node = "/root/.nvm/versions/node/v22.23.2/bin/node"
	}
	state := "set -eu\n" + initial + "\n" + task.inputCheck + "\n" + toolCheck + `
test "$(` + node + ` --version)" = v22.23.2
rg --version | head -1`
	if task.packagesSHA != "" && task.packagesField == "" {
		state += "\ncommand -v git patch perl\ndpkg-query -W python3 git patch perl-base"
	}
	out, err := checked(ctx, m, id, core.Command{Path: "/bin/bash", Args: []string{"-lc", state}, Dir: r.Workdir})
	if err != nil {
		return "", nil, err
	}
	pkgField := task.packagesField
	if pkgField == "" {
		pkgField = "ubuntu_packages_sha256"
	}
	details := map[string]string{"codex_prefix_sha256": codexPrefixSHA, pkgField: task.packagesSHA, "checks": out, "recorded_temp_allocation": allocation}
	if shardSciPy != "" {
		details["scipy_wheel_archive_sha256"] = pretrainShardSciPyArchive
	}
	if hfHarness != "" {
		details["curated_plugins_commit"] = hfPluginsCommit
		details["curated_plugins_archive_sha256"] = hfPluginsArchive
		details["inspection_tool_archive_sha256"] = hfInspectionPackages
	}
	if r.Task == hfTask {
		details["inference_environment_sha256"] = hfEnvironmentSHA
	}
	if grpcTools != "" {
		details["system_tools_archive_sha256"] = grpcSystemToolsSHA
		details["network_profile"] = "ipv4-addresses-only"
	}
	if r.Task == sparqlTask {
		pip, err := prepareSparql(ctx, m, id, r.Trial)
		if err != nil {
			return "", nil, err
		}
		details["pip_configuration"] = strings.TrimSpace(pip)
	}
	if r.Task == torchTask {
		pip, err := prepareTorch(ctx, m, id, r.Trial)
		if err != nil {
			return "", nil, err
		}
		details["pip_configuration"] = strings.TrimSpace(pip)
	}
	if curlPackages != "" {
		details["debian_curl_archive_sha256"] = debianCurlSHA
	}
	if r.Task == "compile-compcert" {
		details["ubuntu_ca_bootstrap_sha256"] = compilerBootstrapSHA
	}
	if r.Trial == datasetPythonTrial {
		details["python_environment_sha256"] = kvPythonSHA
	}
	return "source_task_ready", details, nil
}

func installPackages(ctx context.Context, m *vhive.Manager, id, packages, sha string) error {
	const pkgDir = "/tmp/aries-guest-offline-packages"
	if _, err := checked(ctx, m, id, core.Command{Path: "/bin/mkdir", Args: []string{pkgDir}}); err != nil {
		return err
	}
	if err := uploadPinned(ctx, m, id, packages, sha, pkgDir); err != nil {
		return err
	}
	const install = `set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get -o Dir::Etc::sourcelist=/dev/null -o Dir::Etc::sourceparts=- --no-install-recommends -y install /tmp/aries-guest-offline-packages/*.deb
rm -f -- ./*.deb
cd /tmp
rmdir aries-guest-offline-packages`
	if _, err := checked(ctx, m, id, core.Command{Path: "/bin/sh", Args: []string{"-c", install}, Dir: pkgDir}); err != nil {
		return err
	}
	return nil
}

// This source session refers to the original random name literally in four
// later steps. Allocate its recorded unused directory on the original call.
var recordedTempDirectories = map[string][2]string{
	"1b1460f4-7006-4469-997e-2034ffed4e78": {"/tmp/dclm-secret-audit.XXXXXX", "/tmp/dclm-secret-audit.2hgPQQ"},
	"6d0403e0-740e-4104-a684-619cfc8f85bf": {"/tmp/dclm-sanitize.XXXXXX", "/tmp/dclm-sanitize.qMsklc"},
	"e9635d5f-9991-4e6a-8308-887e68e6cfa6": {"/tmp/dclm-secret-audit.XXXXXX", "/tmp/dclm-secret-audit.E3lIvM"},
	"a841b950-f1ec-48f4-bff2-ff231aba2c25": {"/tmp/dclm-secret-audit.XXXXXX", "/tmp/dclm-secret-audit.iOpZ0E"},
	"337f0b12-666b-4f8f-bd8e-1eec7ab2de7f": {"/tmp/latex-synonyms.XXXXXX", "/tmp/latex-synonyms.eqOYqu"},
	"18bb015b-80fc-4347-9743-2c11bacc0bcc": {"/tmp/latex-synonyms.XXXXXX", "/tmp/latex-synonyms.5bqpU2"},
	"0ddfe8fb-e4d8-44f5-970e-27dcb8ec2999": {"/tmp/latex-synonyms.XXXXXXXX", "/tmp/latex-synonyms.RFhrThu1"},
	"786300a5-e689-4b20-ada9-edd20999f11e": {"/tmp/university-sparql-validation.XXXXXX", "/tmp/university-sparql-validation.7tKi8V"},
	"f2d28757-76b8-423d-8f9b-e64b5fb61aa7": {"/tmp/sparql-tools.XXXXXX", "/tmp/sparql-tools.zyCf5e"},
}

const installRecordedTempAllocation = `set -eu
test ! -e SOURCE_DIR
test ! -e /usr/local/bin/mktemp
cat > /usr/local/bin/mktemp <<'ARIESMKTEMP'
#!/bin/sh
if [ "$#" -eq 2 ] && [ "$1" = "-d" ] && [ "$2" = "SOURCE_TEMPLATE" ]; then
    umask 077
    mkdir -m 700 -- SOURCE_DIR || exit 1
    printf '%s\n' SOURCE_DIR
else
    exec /usr/bin/mktemp "$@"
fi
ARIESMKTEMP
chmod 0755 /usr/local/bin/mktemp
sha256sum /usr/local/bin/mktemp
printf '%s\n' 'SOURCE_TEMPLATE -> SOURCE_DIR'`

func captureTaskOutput(ctx context.Context, m *vhive.Manager, id string, r *harbor.Recording, log *json.Encoder, grpcTools bool) error {
	task := taskSetups[r.Task]
	if r.Task == nextjsTask {
		task, _ = nextjsSourceSetup(r.Trial)
	}
	begin, err := harbor.Clock()
	if err != nil {
		return err
	}
	cmd := "set -eu\ntest -s " + task.output + "\nsha256sum " + task.output + "\n" + task.inputCheck
	if r.Trial == pretrainShardLiveTrial {
		cmd = "set -eu\n" + pretrainShardLiveReadback + cmd
	}
	if r.Task == hfTask {
		cmd += hfFinalCheck(r.Trial)
	}
	if r.Task == "mvcc-lsm-compaction" {
		// Each digest covers all fourteen files retained in the original trial.
		want := map[string]string{
			"142e80d4-a690-4aa9-8722-7b02a585a936": "2e2779e3df3f5ebc272276c72b0b5ec3e2955838fcca61fc3786c3d1841d23e4",
			"88f11121-1f97-443d-9a33-d28ec9956a0d": "044f597b19b55320f6242df54f32871187312c66e7ac2af7468c7705d2e37ecf",
			"759604be-a617-4ac5-be5e-289cdc771b9d": "6bb69077e527de9e654667d5b9c6b21d387eb8a1edeb8c1ad190aa14ed3df314",
			"6a70afa0-fb22-4646-b2bb-3ff8eced7237": "4b606e96f191e952f086ad5cd7edcff00ec7450b8b28455c9a6e1f5e50157f90",
			"ef50d0ac-9de1-4c3d-b320-3312179b5ca1": "b0ce0d81f7f00162d58f3571b42124488921731fd4d50390972160700232840a",
		}[r.Trial]
		cmd += "\nexport LC_ALL=C\nsha256sum Makefile crash_report.txt include/mvcc_lsm/* src/* tests/* | tee /dev/stderr | sha256sum | grep -Fx '" + want + "  -'"
	}
	if r.Task == "freight-dispatch-shift" {
		// Original Harbor artifact for each source-specific dispatcher.
		digest := map[string]string{
			"3dc97e08-94f1-44a9-baa2-81629ba79eab": "80e2c0644bb5b7edacfce3445b23ca3995cc79fd6376b9a3cdcc3bcd0b442cbd",
			"fa1d8385-d7d0-4701-b73e-024703e6b34a": "c9b3af973c3ae78b1f16e9a84066676136d6c62a6d1206ee260cabded38f6363",
			"ea7c7508-199a-4a32-ac02-a17ae1a80cd1": "174c984b82d21161717fa4f31ac02983c95e4e08d9da85e56cdfb250a738de05",
			"f482db32-1965-4c26-b403-9a6959d49457": "e2c172bba6c6b665c23ee9d1df425f079a6c913cfc3403bdba5af62d768466b9",
			"09697b02-59a2-4588-b1f6-fab871a5c35e": "ea6329a2d1d960fb63c325af202120649f56226faea8a5adcf9b635e485c5023",
		}[r.Trial]
		cmd += "\necho '" + digest + "  /workspace/dispatch' | sha256sum -c -"
	}
	if r.Task == paymentsTask {
		cmd += paymentsFinalCheck
		if r.Trial == harbor.PaymentsRejectedPatchTrial {
			cmd += "test ! -e state.py\n"
		} else {
			cmd += "sha256sum state.py\n"
		}
	}
	switch r.Trial {
	case "cfff4007-c001-4e29-950d-eb1a1e8a7bb0":
		cmd += intrastatFirstFinal
	case "506d9672-f546-46ed-8232-37ed8009fe96":
		cmd += strings.ReplaceAll(intrastatLaterFinal, "EXPECTED_STABLE", "0b56c06fde361bcedac3fa1634e2947fcc0e22a68e7257317c9ea703211bd6e9")
	case "b2e0b98c-215e-48ab-aece-c16b1420b59a":
		cmd += strings.ReplaceAll(intrastatLaterFinal, "EXPECTED_STABLE", "ec6857941506ecff75853f02372bbf3ac50b20cf802bfd0f8494036df585e0e4")
	case "0e67fd64-d0a0-4afb-9afe-17e48f2f0719":
		cmd += strings.ReplaceAll(intrastatLaterFinal, "EXPECTED_STABLE", "3ff63377a34299c22028fc2f4c25fc6554ab4eb3af15448199de77deb30c2d60")
	case "1a3d9576-5102-4784-a660-6129aedee85c":
		cmd += strings.ReplaceAll(intrastatLaterFinal, "EXPECTED_STABLE", "6c6d600f157417e8f6d29c909ec54720342f6660ad9be1c7c738d5614dd797dc")
	case nextjsTrial:
		cmd += nextjsFinalCheck
	case nextjsWave2Trial:
		cmd += nextjsWave2FinalCheck
	case nextjsWave3Trial:
		cmd += nextjsWave3FinalCheck + nextjsLaterFinalCheck
	case nextjsWave4Trial:
		cmd += nextjsWave4FinalCheck + nextjsLaterFinalCheck
	case nextjsWave5Trial:
		cmd += nextjsWave5FinalCheck + nextjsLaterFinalCheck
	}
	if r.Task == "sanitize-git-repo" {
		cmd += "\nfind . -type f -not -path './.git/*' -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum"
	}
	if r.Trial == hofExperimentalTrial {
		cmd += `
echo '320537a03e8783317457a88d17ee0137f80fd0c3eb914da56c8a76b50a379386  /app/solution/Systre.jar' | sha256sum -c
cd /app/solution
/opt/venv/bin/python validate.py`
	}
	if r.Trial == hofGavrogTrial {
		cmd += hofGavrogFinal
	}
	if r.Trial == hofReleaseTrial {
		cmd += hofReleaseFinal
	}
	if r.Trial == hofSystreTrial {
		cmd += `
sha256sum -c <<'SYSTRE'
0d272e98a1a21669bc67a809b95c014ba2a2fb39a6fd2147039201216ab0d48d  /app/solution/tools/Systre.jar
e09244a29e8299ea7e47e6d10778c1cfecc49bbab77ee702ce592662715fcb60  /app/solution/tools/SystreCmdline.java
SYSTRE
java -version
dpkg-query -W openjdk-21-jre-headless default-jre-headless`
	}
	if r.Task == "embedding-drift-monitor" {
		cmd += `
test "$(sha256sum drift_monitor/*.py | sha256sum)" != 'a321e7570ddc8e9b6220dc3bd2e9c38b209eb49ab0398558e8dd521d0c27157c  -'
sha256sum drift_monitor/*.py`
	}
	if r.Task == bipedTask {
		cmd += bipedFinalCheck
	}
	if r.Task == pretrainShardTask {
		if r.Trial == pretrainShardSciPyTrial {
			cmd += pretrainShardSciPyFinal
		}
		cmd += pretrainShardFinal
	}
	if r.Task == vppLossTask {
		cmd += vppLossFinal
	}
	if r.Task == soundCascadeTask {
		cmd += soundCascadeFinal
	}
	if r.Task == markerTask {
		cmd += markerFinal(r.Trial)
	}
	if r.Task == financialTask {
		cmd += financialFinalCheck
		if profile, ok := financialAptTrials[r.Trial]; ok {
			cmd += financialAptCheck(profile)
		} else if r.Trial == financialMuPDFTrial {
			cmd += "\n/tmp/document-processing-env/bin/python -c 'from importlib.metadata import version; assert version(\"PyMuPDF\") == \"1.28.2\"'\n"
		} else {
			cmd += financialPypdfCheck
		}
	}
	if r.Task == kvTask {
		check := kvFinalCheck(r.Trial)
		if grpcTools {
			check = strings.ReplaceAll(check, "/tmp/aries-kv-install.json", grpcReportPath)
		}
		cmd += check
	}
	if r.Task == sparqlTask {
		check, err := sparqlOutputCheck(r.Trial)
		if err != nil {
			return err
		}
		cmd += check
	}
	if r.Task == torchTask {
		check, err := torchOutputCheck(r.Trial)
		if err != nil {
			return err
		}
		cmd += check
	}
	if r.Task == datasetTask {
		cmd += datasetFinal
	}
	if r.Task == claimsTask {
		check, err := claimsFinalCheck(r.Trial)
		if err != nil {
			return err
		}
		cmd += check
	}
	if r.Task == "compile-compcert" {
		cmd += "\n" + compilerResult(r.Trial)
	}
	if r.Task == "video-processing" && r.Trial != videoPatchTrial {
		cmd += `
python - <<'PYREPORT'
import json
from pathlib import Path
records = json.loads(Path('/tmp/aries-reviewed-pip-install.json').read_text())['install']
for item in records:
    print(json.dumps({'name':item['metadata']['name'],'version':item['metadata']['version'],'download':item['download_info']},sort_keys=True))
PYREPORT`
	}
	out, err := checked(ctx, m, id, core.Command{Path: "/bin/bash", Args: []string{"-lc", cmd}, Dir: r.Workdir})
	end, clockErr := harbor.Clock()
	if err != nil || clockErr != nil {
		return errors.Join(err, clockErr)
	}
	return log.Encode(map[string]any{"event": "final_task_state", "checks": strings.TrimSpace(out), "begin_monotonic_ns": begin, "end_monotonic_ns": end})
}

func networkEvidence(ctx context.Context, m *vhive.Manager, id, phase string, recording *harbor.Recording, log *json.Encoder) error {
	task := recording.Task
	const pypiProbe = `import json, socket, urllib.request
from pathlib import Path
with urllib.request.urlopen("https://pypi.org/simple/toml/",timeout=10) as response:
    print(json.dumps({"route":Path("/proc/net/route").read_text(),"resolv_conf":Path("/etc/resolv.conf").read_text(),"pypi_addresses":sorted({a[4][0] for a in socket.getaddrinfo("pypi.org",443)}),"pypi_status":response.status}))`
	begin, err := harbor.Clock()
	if err != nil {
		return err
	}
	python, probe := "/usr/local/bin/python", pypiProbe
	if task == hfTask {
		probe = hfNetworkProbe
	}
	if task == markerTask {
		python = "/usr/bin/python3"
		probe = fmt.Sprintf(`import json,urllib.request
with urllib.request.urlopen("https://snapshot.ubuntu.com/ubuntu/%s/dists/noble/InRelease",timeout=15) as r:
    print(json.dumps({"dependencies":{"snapshot.ubuntu.com":{"status":r.status}}}))`, markerTrials[recording.Trial].stamp)
	}
	if task == financialTask {
		python = "/usr/bin/python3"
	}
	if recording.Trial == hofSystreTrial || recording.Trial == hofExperimentalTrial || recording.Trial == hofGavrogTrial || recording.Trial == hofReleaseTrial {
		python = "/opt/venv/bin/python"
		probe = `import json,socket,urllib.request
hosts = ["api.github.com","github.com","raw.githubusercontent.com","release-assets.githubusercontent.com","snapshot.ubuntu.com"]
with urllib.request.urlopen(urllib.request.Request("https://snapshot.ubuntu.com/ubuntu/20260831T073500Z/dists/noble/InRelease",method="HEAD"),timeout=15) as response:
    print(json.dumps({"snapshot_status":response.status,"addresses":{h:sorted({a[4][0] for a in socket.getaddrinfo(h,443)}) for h in hosts}}))`
	}
	if recording.Trial == pretrainShardLiveTrial {
		probe = pretrainShardLiveNetwork
	}
	if task == datasetTask {
		// This recording times both mutable API URLs at step 7. Its own guest
		// process should make those requests first on a freshly created VM.
		mode := "False"
		if recording.Trial == datasetPythonTrial {
			mode = "True"
		}
		probe = "use_revision_endpoint = " + mode + "\n" + datasetNetworkProbe
	}
	if task == "compile-compcert" {
		python, probe = "/usr/bin/python3", compilerNetworkCheck(recording.Trial, recording.Plan.StartedAt)
	}
	if task == sparqlTask {
		python, probe = "/usr/bin/python3", sparqlNetworkProbe
	}
	if task == torchTask {
		python, probe = "/usr/bin/python3", torchNetworkProbe
	}
	if task == paymentsTask {
		probe = paymentsNetworkProbe
	}
	command := core.Command{Path: python, Args: []string{"-c", probe}}
	if recording.Task == nextjsTask {
		command = core.Command{Path: "/usr/local/bin/node", Args: []string{"-e", nextjsHealthCheck}}
	}
	out, err := checked(ctx, m, id, command)
	end, clockErr := harbor.Clock()
	if err != nil || clockErr != nil || !json.Valid([]byte(out)) {
		if task == paymentsTask {
			return errors.Join(errors.New("local Payments fixture health readback failed"), err, clockErr)
		}
		if recording.Task == nextjsTask {
			return errors.Join(errors.New("warehouse companion health readback failed"), err, clockErr)
		}
		return errors.Join(errors.New("public dependency route/DNS/TLS readback failed"), err, clockErr)
	}
	return log.Encode(map[string]any{"event": "validated_guest_network", "phase": phase, "begin_monotonic_ns": begin, "end_monotonic_ns": end, "network": json.RawMessage(out)})
}
