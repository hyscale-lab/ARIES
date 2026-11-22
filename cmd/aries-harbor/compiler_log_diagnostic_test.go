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
