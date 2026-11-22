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

const compilerLogTrial = "bf3c1acf-2d7d-4fa4-9b03-fb8edfce1666"
const compilerLogListing = "call_OzOc4ZyYPdULwaa1wFz54414"
const sourceCoqLog = "/root/.opam/log/coq-10828-f5dcbf.out"

var coqLogName = regexp.MustCompile(`^(coq-[0-9]+-[0-9a-f]{6})\.(out|info|env)$`)

func listedCoqLog(output string) (string, error) {
	prefix, listing, ok := strings.Cut(output, "\nCurrent OPAM logs\n")
	if !ok || !strings.Contains(prefix, "retrieved coq.8.16.1") {
		return "", fmt.Errorf("OPAM package retrieval and directory listing are required")
	}
	stem, err := listedCoqFiles(listing)
	if err != nil {
		return "", err
	}
	return "/root/.opam/log/" + stem + ".out", nil
}

func listedCoqFiles(listing string) (string, error) {
	var stem string
	found := make(map[string]bool)
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "-rw-r--r--" {
			continue
		}
		match := coqLogName.FindStringSubmatch(fields[len(fields)-1])
		if match == nil {
			continue
		}
		if (stem != "" && stem != match[1]) || found[match[2]] {
			return "", fmt.Errorf("ambiguous live Coq log series")
		}
		stem = match[1]
		found[match[2]] = true
	}
	if len(found) != 3 {
		return "", fmt.Errorf("OPAM output, environment and metadata files were not all observed")
	}
	return stem, nil
}

type compilerGuestExec func(context.Context, replay.Call) (core.CommandResult, error)

func mapCompilerLog(exec compilerGuestExec, record func(any) error) compilerGuestExec {
	var live string
	return func(ctx context.Context, call replay.Call) (core.CommandResult, error) {
		switch call.ToolCallID {
		case compilerLogListing:
			out, err := exec(ctx, call)
			if err != nil {
				return out, err
			}
			historical, err := listedCoqLog(string(call.ResponseStdout))
			if err != nil || historical != sourceCoqLog {
				return out, fmt.Errorf("source OPAM log listing differs")
			}
			live, err = listedCoqLog(out.Stdout)
			if err != nil {
				return out, err
			}
			at, err := harbor.Clock()
			if err != nil {
				return out, err
			}
			return out, record(map[string]any{"event": "diagnostic_opam_log_discovery", "monotonic_ns": at,
				"source_call_id": call.ToolCallID, "sequence": call.Sequence, "source_path": historical, "live_path": live})
		case "call_HLvLcHhGgMXPgnBcIDrhu0id", "call_JfLG7dJwGS1CpH9UO0JZ2sVT",
			"call_apmIF7ZRE8fFykpRfd69oosU", "call_VLZa40BHyyL377Vm1yOFTG8G", "call_4UXzneDxqNRlHO3ftaol4ANo":
			if live == "" || strings.Count(call.Script, sourceCoqLog) != 1 {
				return core.CommandResult{}, fmt.Errorf("missing unique observed Coq log binding")
			}
			original := call.Script
			call.Script = strings.Replace(original, sourceCoqLog, live, 1)
			at, err := harbor.Clock()
			if err != nil {
				return core.CommandResult{}, err
			}
			if err = record(map[string]any{"event": "diagnostic_opam_log_read", "monotonic_ns": at,
				"source_call_id": call.ToolCallID, "sequence": call.Sequence, "source_path": sourceCoqLog, "live_path": live,
				"source_script_sha256":   fmt.Sprintf("%x", sha256.Sum256([]byte(original))),
				"executed_script_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(call.Script)))}); err != nil {
				return core.CommandResult{}, err
			}
		}
		return exec(ctx, call)
	}
}
