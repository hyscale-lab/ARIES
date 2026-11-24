package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

const bnFitTask = "bn-fit-modify"

var bnFitSetup = taskSetup{
	initialCheck: `echo '7d4c27aca2f7443500f993bf0cc31fc7c7e4ef45bb5370ba890848ee1ef3cb04  bn_sample_10k.csv' | sha256sum -c -
test ! -e learned_dag.csv && test ! -e intervened_dag.csv && test ! -e final_bn_sample.csv
Rscript --version
python3 --version`,
}

const bnFitReadArtifacts = `
import base64
import gzip
import hashlib
import json
from pathlib import Path
import stat

def capture(name, export):
    path = Path(name)
    try:
        st = path.lstat()
    except FileNotFoundError:
        return {"state": "missing"}
    if not stat.S_ISREG(st.st_mode):
        return {"state": "not_regular"}
    with path.open("rb") as stream:
        data = stream.read((2 << 20) + 1)
    if len(data) > 2 << 20:
        return {"state": "oversize", "bytes": st.st_size}
    result = {"state": "captured", "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
    if export:
        result["encoding"] = "gzip+base64"
        result["data"] = base64.b64encode(gzip.compress(data, mtime=0)).decode("ascii")
    return result

files = ("learned_dag.csv", "intervened_dag.csv", "final_bn_sample.csv")
print(json.dumps({"input": capture("bn_sample_10k.csv", False),
                  "outputs": {name: capture(name, True) for name in files}}))
`

// Capture the actual application files after the measured session has ended.
// The independent benchmark verifier evaluates these bytes outside the guest.
func captureBnFitArtifacts(ctx context.Context, m *vhive.Manager, id string, r *harbor.Recording, log *json.Encoder) error {
	begin, err := harbor.Clock()
	if err != nil {
		return err
	}
	out, execErr := m.Exec(ctx, id, core.Command{
		Path: "/usr/bin/python3", Args: []string{"-c", bnFitReadArtifacts}, Dir: r.Workdir,
		Timeout: 30 * time.Second, OutputLimitBytes: 10 << 20,
	})
	end, err := harbor.Clock()
	if err != nil {
		return errors.Join(execErr, err)
	}
	evidence := map[string]any{
		"event": "bn_fit_artifacts", "trial_id": r.Trial, "begin_monotonic_ns": begin,
		"end_monotonic_ns": end, "capture_exit_code": out.ExitCode,
	}
	if execErr != nil || out.ExitCode != 0 || !json.Valid([]byte(out.Stdout)) {
		evidence["capture_error"] = "guest application file capture failed"
		return errors.Join(log.Encode(evidence), execErr)
	}
	evidence["files"] = json.RawMessage(out.Stdout)
	return log.Encode(evidence)
}
