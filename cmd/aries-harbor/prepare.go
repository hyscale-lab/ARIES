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
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

const codexPrefixSHA = "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab"

type taskSetup struct {
	packagesSHA, output, inputCheck, initialCheck string
}

var taskSetups = map[string]taskSetup{
	sparqlTask: sparqlSetup,
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

func prepareTask(ctx context.Context, m *vhive.Manager, id string, r *harbor.Recording, tools, packages string) (string, any, error) {
	if r.Task == "fix-git" {
		out, err := checked(ctx, m, id, core.Command{Path: "/usr/local/bin/python3", Args: []string{"-c", initialGitState}, Dir: r.Workdir})
		if err != nil || !json.Valid([]byte(out)) {
			return "", nil, errors.Join(errors.New("restore source Git starting state"), err)
		}
		return "source_git_ready", json.RawMessage(out), nil
	}
	task, ok := taskSetups[r.Task]
	if !ok {
		return "", nil, fmt.Errorf("no ready-state definition for %s", r.Task)
	}
	if task.packagesSHA != "" {
		if err := installPackages(ctx, m, id, packages, task.packagesSHA); err != nil {
			return "", nil, err
		}
	}
	if err := uploadPinned(ctx, m, id, tools, codexPrefixSHA, "/"); err != nil {
		return "", nil, err
	}
	if r.Task == "code-from-image" || r.Task == "circuit-fibsqrt" || r.Task == "video-processing" {
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
	if r.Trial == "b43d6be8-70d4-44fb-97a6-358b6bb55b3f" {
		// This recorded ls requires the harness directory. Its transient logs
		// and credential-bearing state are deliberately absent from the fixture.
		if _, err := checked(ctx, m, id, core.Command{Path: "/bin/mkdir", Args: []string{"-m", "0700", "-p", "/tmp/codex-home"}}); err != nil {
			return "", nil, err
		}
	}
	initial := task.initialCheck
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
	state := "set -eu\n" + initial + "\n" + task.inputCheck + `
command -v node codex rg python3
test "$(node --version)" = v22.23.2
rg --version | head -1`
	if task.packagesSHA != "" {
		state += "\ncommand -v git patch perl\ndpkg-query -W python3 git patch perl-base"
	}
	out, err := checked(ctx, m, id, core.Command{Path: "/bin/bash", Args: []string{"-lc", state}, Dir: r.Workdir})
	if err != nil {
		return "", nil, err
	}
	details := map[string]string{"codex_prefix_sha256": codexPrefixSHA, "ubuntu_packages_sha256": task.packagesSHA, "checks": out, "recorded_temp_allocation": allocation}
	if r.Task == sparqlTask {
		pip, err := prepareSparql(ctx, m, id, r.Trial)
		if err != nil {
			return "", nil, err
		}
		details["pip_configuration"] = strings.TrimSpace(pip)
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

func captureTaskOutput(ctx context.Context, m *vhive.Manager, id string, r *harbor.Recording, log *json.Encoder) error {
	task := taskSetups[r.Task]
	begin, err := harbor.Clock()
	if err != nil {
		return err
	}
	cmd := "set -eu\ntest -s " + task.output + "\nsha256sum " + task.output + "\n" + task.inputCheck
	if r.Task == sparqlTask {
		check, err := sparqlOutputCheck(r.Trial)
		if err != nil {
			return err
		}
		cmd += check
	}
	if r.Task == "video-processing" {
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

func networkEvidence(ctx context.Context, m *vhive.Manager, id, phase, task string, log *json.Encoder) error {
	const pypiProbe = `import json, socket, urllib.request
from pathlib import Path
with urllib.request.urlopen("https://pypi.org/simple/toml/",timeout=10) as response:
    print(json.dumps({"route":Path("/proc/net/route").read_text(),"resolv_conf":Path("/etc/resolv.conf").read_text(),"pypi_addresses":sorted({a[4][0] for a in socket.getaddrinfo("pypi.org",443)}),"pypi_status":response.status}))`
	begin, err := harbor.Clock()
	if err != nil {
		return err
	}
	python, probe := "/usr/local/bin/python", pypiProbe
	if task == sparqlTask {
		python, probe = "/usr/bin/python3", sparqlNetworkProbe
	}
	out, err := checked(ctx, m, id, core.Command{Path: python, Args: []string{"-c", probe}})
	end, clockErr := harbor.Clock()
	if err != nil || clockErr != nil || !json.Valid([]byte(out)) {
		return errors.Join(errors.New("public dependency route/DNS/TLS readback failed"), err, clockErr)
	}
	return log.Encode(map[string]any{"event": "validated_guest_network", "phase": phase, "begin_monotonic_ns": begin, "end_monotonic_ns": end, "network": json.RawMessage(out)})
}
