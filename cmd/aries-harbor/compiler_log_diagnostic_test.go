package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func testCoqListing(stem string) string {
	out := "\n-> retrieved coq.8.16.1\nCurrent OPAM logs\n"
	for _, ext := range []string{"out", "info", "env"} {
		out += fmt.Sprintf("-rw-r--r-- 1 root root 10 Nov 1 09:00 %s.%s\n", stem, ext)
	}
	return out
}

func TestCompilerLogDiagnosticUsesOneObservedTrio(t *testing.T) {
	ctx := context.Background()
	source := testCoqListing("coq-10828-f5dcbf")
	live := testCoqListing("coq-3378-ea0ff1")
	var executed []string
	var observations []any
	exec := mapCompilerLog(func(_ context.Context, call replay.Call) (core.CommandResult, error) {
		executed = append(executed, call.Script)
		return core.CommandResult{Stdout: live}, nil
	}, func(v any) error { observations = append(observations, v); return nil })
	read := replay.Call{ToolCallID: "call_JfLG7dJwGS1CpH9UO0JZ2sVT", Script: "tail -n 12 " + sourceCoqLog}
	if _, err := exec(ctx, read); err == nil {
		t.Fatal("read before discovery was accepted")
	}
	if _, err := exec(ctx, replay.Call{ToolCallID: compilerLogListing, ResponseStdout: []byte(source)}); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(ctx, read); err != nil {
		t.Fatal(err)
	}
	if executed[1] != "tail -n 12 /root/.opam/log/coq-3378-ea0ff1.out" || len(observations) != 2 {
		t.Fatal(executed, observations)
	}
	read.ToolCallID = "another-source-call"
	if _, err := exec(ctx, read); err != nil || executed[2] != read.Script {
		t.Fatal("outside the reviewed read set", err, executed)
	}
	for _, invalid := range []string{strings.Replace(live, ".env", ".bad", 1), live + live, strings.Replace(live, "3378", "22", 1)} {
		if _, err := listedCoqLog(invalid); err == nil {
			t.Fatal("ambiguous or incomplete Coq log accepted")
		}
	}
}

func TestDiagnosticInstallerPIDAndLogMustAgree(t *testing.T) {
	const format = "%s 10121 01:04 7.6 0.1 opam opam install --switch=compcert-3.13.1 --jobs=2 coq.8.16.1 menhir.20230608 dune.3.15.3\n"
	sourcePS, livePS := fmt.Sprintf(format, "10691"), fmt.Sprintf(format, "3238")
	sourceFiles, liveFiles := testCoqListing(originalCoqBuildStem), testCoqListing("coq-3238-58bc74")
	var last string
	exec := mapCompilerWait(func(_ context.Context, call replay.Call) (core.CommandResult, error) {
		last = call.Script
		switch call.ToolCallID {
		case "call_FwQedwl1g7q1DZooMva0h7DK":
			return core.CommandResult{Stdout: livePS}, nil
		case "call_Fc8R3gqnH98Oonmr8TZa6TLF":
			return core.CommandResult{Stdout: liveFiles}, nil
		}
		return core.CommandResult{}, nil
	}, func(any) error { return nil })
	discover := replay.Call{ToolCallID: "call_FwQedwl1g7q1DZooMva0h7DK", ResponseStdout: []byte(sourcePS)}
	list := replay.Call{ToolCallID: "call_Fc8R3gqnH98Oonmr8TZa6TLF", ResponseStdout: []byte(sourceFiles)}
	check := replay.Call{ToolCallID: "call_XvxnxIIgvuE5dTlYGPEXcnEr", Script: "while kill -0 10691 2>/dev/null; do sleep 10; done"}
	read := replay.Call{ToolCallID: "call_Ene8L95LHhXkOg48Fefv9dFv", Script: "tail -n 2 " + coqBuildLogDir + originalCoqBuildStem + ".out"}
	for _, call := range []replay.Call{discover, list, check} {
		if _, err := exec(t.Context(), call); err != nil {
			t.Fatal(err)
		}
	}
	if last != "while kill -0 3238 2>/dev/null; do sleep 10; done" {
		t.Fatal(last)
	}
	if _, err := exec(t.Context(), read); err != nil || last != "tail -n 2 /tmp/compcert-opam/log/coq-3238-58bc74.out" {
		t.Fatal(last, err)
	}
	liveFiles = testCoqListing("coq-9999-58bc74")
	if _, err := exec(t.Context(), list); err == nil {
		t.Fatal("accepted log belonging to another installer")
	}
	livePS += fmt.Sprintf(format, "4001")
	if _, err := exec(t.Context(), discover); err == nil {
		t.Fatal("accepted ambiguous installer command line")
	}
	check.Script = "while kill -9 10691 2>/dev/null; do sleep 10; done"
	if _, err := exec(t.Context(), check); err == nil {
		t.Fatal("accepted a changed liveness check")
	}
}
