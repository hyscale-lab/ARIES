package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

const compilerWaitTrial = "921ac918-1cd2-41af-858a-a37490ced8f4"
const coqBuildLogDir = "/tmp/compcert-opam/log/"
const originalCoqBuildStem = "coq-10691-f6c36c"

// Match the full installer argv and command name from the original ps fields.
var opamInstaller = regexp.MustCompile(`(?m)^\s*([1-9][0-9]*)\s+[0-9]+\s+\S+\s+[0-9.]+\s+[0-9.]+\s+opam\s+opam install --switch=compcert-3\.13\.1 --jobs=2 coq\.8\.16\.1 menhir\.20230608 dune\.3\.15\.3$`)

func mapCompilerWait(exec compilerGuestExec, record func(any) error) compilerGuestExec {
	var pid, stem string
	return func(ctx context.Context, call replay.Call) (core.CommandResult, error) {
		receipt := map[string]any{"source_call_id": call.ToolCallID, "sequence": call.Sequence}
		save := func(kind string) error {
			at, err := harbor.Clock()
			if err != nil {
				return err
			}
			receipt["event"], receipt["monotonic_ns"] = kind, at
			return record(receipt)
		}
		var literal, replacement, kind string
		switch call.ToolCallID {
		case "call_FwQedwl1g7q1DZooMva0h7DK":
			out, err := exec(ctx, call)
			if err != nil {
				return out, err
			}
			original := opamInstaller.FindAllStringSubmatch(string(call.ResponseStdout), -1)
			actual := opamInstaller.FindAllStringSubmatch(out.Stdout, -1)
			if len(original) != 1 || original[0][1] != "10691" || len(actual) != 1 {
				return out, fmt.Errorf("missing a unique OPAM installer with the original command line")
			}
			pid = actual[0][1]
			receipt["source_pid"], receipt["live_pid"] = original[0][1], pid
			return out, save("diagnostic_opam_pid_discovery")
		case "call_Fc8R3gqnH98Oonmr8TZa6TLF":
			out, err := exec(ctx, call)
			if err != nil {
				return out, err
			}
			original, sourceErr := listedCoqFiles(string(call.ResponseStdout))
			actual, liveErr := listedCoqFiles(out.Stdout)
			if pid == "" || sourceErr != nil || liveErr != nil || original != originalCoqBuildStem || !strings.HasPrefix(actual, "coq-"+pid+"-") {
				return out, fmt.Errorf("Coq log trio does not match the previously observed installer")
			}
			stem = actual
			receipt["source_path"], receipt["live_path"] = coqBuildLogDir+original+".out", coqBuildLogDir+stem+".out"
			return out, save("diagnostic_opam_log_discovery")
		case "call_XvxnxIIgvuE5dTlYGPEXcnEr":
			if pid == "" || stem == "" {
				return core.CommandResult{}, fmt.Errorf("installer and its Coq log must be observed before the liveness check")
			}
			literal, replacement = "while kill -0 10691 2>/dev/null;", "while kill -0 "+pid+" 2>/dev/null;"
			kind = "diagnostic_opam_pid_check"
			receipt["source_pid"], receipt["live_pid"] = "10691", pid
		case "call_Ene8L95LHhXkOg48Fefv9dFv", "call_xUZRvssrXY8zlo4oCZc0dS8F":
			if stem == "" {
				return core.CommandResult{}, fmt.Errorf("Coq log must be observed before a mapped progress read")
			}
			literal, replacement = coqBuildLogDir+originalCoqBuildStem+".out", coqBuildLogDir+stem+".out"
			kind = "diagnostic_opam_log_read"
			receipt["source_path"], receipt["live_path"] = literal, replacement
		default:
			return exec(ctx, call)
		}
		if strings.Count(call.Script, literal) != 1 {
			return core.CommandResult{}, fmt.Errorf("diagnostic operand does not occur exactly once")
		}
		receipt["source_script_sha256"] = fmt.Sprintf("%x", sha256.Sum256([]byte(call.Script)))
		call.Script = strings.Replace(call.Script, literal, replacement, 1)
		receipt["executed_script_sha256"] = fmt.Sprintf("%x", sha256.Sum256([]byte(call.Script)))
		if err := save(kind); err != nil {
			return core.CommandResult{}, err
		}
		return exec(ctx, call)
	}
}
