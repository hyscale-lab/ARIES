package harbor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNestedPatchSourceUsesNativePatch(t *testing.T) {
	const patch = "*** Begin Patch\n*** Add File: note.txt\n+sample\n*** End Patch"
	const command = "apply_patch <<'PATCH'\n" + patch + "\nPATCH"
	const stdout = "Success. Updated the following files:\nA note.txt\n"
	const reply = "Wall time: 0.0000 seconds\nOutput:\nExit code: 0\nWall time: 0 seconds\nOutput:\n" + stdout
	for _, test := range []struct {
		name, script, workdir, output, operation string
	}{
		{"intercepted", command, "/app", reply, "ApplyPatch"},
		{"ordinary shell", command, "/app", "Process exited with code 0\nOutput:\n" + reply, "Exec"},
		{"extra shell command", command + "\nprintf sample", "/app", reply, ""},
		{"premature delimiter", strings.Replace(command, "+sample", "+sample\nPATCH\nprintf sample", 1), "/app", reply, ""},
		{"unclean directory", command, "/app/../other", reply, ""},
		{"verification failure", command, "/app", "apply_patch verification failed: invalid patch: multiple operations target note.txt", ""},
		{"unexpected nested output", command, "/app", strings.Replace(reply, stdout, "unknown", 1), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			args, err := json.Marshal(map[string]string{"cmd": test.script})
			if err != nil {
				t.Fatal(err)
			}
			call := indexedCall{step: 1, call: toolCall{ID: "test", Name: "exec_command", Args: args}, output: test.output}
			got, err := guestCall(call, test.workdir, time.Time{}, time.Time{}, 1)
			if test.operation == "" {
				if err == nil {
					t.Fatal("ambiguous patch reply was accepted")
				}
				return
			}
			if err != nil || got.Operation != test.operation || got.Script != test.script || got.Workdir != test.workdir {
				t.Fatalf("parsed call %+v, err %v", got, err)
			}
			if got.Operation == "ApplyPatch" && (string(got.Body) != patch || string(got.ResponseStdout) != stdout) {
				t.Fatalf("patch body and reply were changed: %q, %q", got.Body, got.ResponseStdout)
			}
		})
	}
}

func TestSourceProcessRejectionKeepsOriginalInterval(t *testing.T) {
	const args = `{"cmd":"printf sample"}`
	const rendered = "/bin/bash -lc 'printf sample'"
	const shellReply = "Process exited with code 0\nOutput:\nsample"
	refusal := func(renderedReason string) string {
		reason := "`" + renderedReason + "` rejected: fixture policy"
		return fmt.Sprintf("exec_command failed for `%s`: CreateProcess { message: %q }", rendered, fmt.Sprintf("Rejected(%q)", reason))
	}
	parseSource := func(atifReply, nativeReply string) (*Recording, error) {
		atif := fmt.Sprintf(`{"schema_version":"ATIF-v1.7","session_id":"session","agent":{"name":"codex","model_name":"gpt-6-astra"},"steps":[
{"step_id":1,"timestamp":"2026-01-01T00:00:01Z","tool_calls":[{"tool_call_id":"blocked","function_name":"exec_command","arguments":%s}],"observation":{"results":[{"source_call_id":"blocked","content":%q}]}},
{"step_id":2,"timestamp":"2026-01-01T00:00:02Z","tool_calls":[{"tool_call_id":"completed","function_name":"exec_command","arguments":%s}],"observation":{"results":[{"source_call_id":"completed","content":%q}]}}]}`, args, atifReply, args, shellReply)
		native := fmt.Sprintf(`{"type":"session_meta","payload":{"session_id":"session"}}
{"type":"turn_context","payload":{"cwd":"/app"}}
{"timestamp":"2026-01-01T00:00:00Z","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"blocked","arguments":%q}}
{"timestamp":"2026-01-01T00:00:01.05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"blocked","output":%q}}
{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"completed","arguments":%q}}
{"timestamp":"2026-01-01T00:00:02.05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"completed","output":%q}}
{"timestamp":"2026-01-01T00:00:03Z","type":"event_msg","payload":{"type":"task_complete"}}`, args, nativeReply, args, shellReply)
		return parse(workload{Workdir: "/app", Tools: []string{"exec_command"}}, "example", []byte(atif), []byte(native))
	}

	reply := refusal(rendered)
	got, err := parseSource(reply, reply)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Plan.Calls) != 1 || got.Plan.Calls[0].ToolCallID != "completed" {
		t.Fatalf("unexpected guest calls: %+v", got.Plan.Calls)
	}
	request, response := got.Events[1], got.Events[2]
	if request.Kind != "harness_rejection_request" || request.Sequence != 0 || response.Kind != "harness_rejection_reply" || response.Sequence != 0 ||
		response.Wait != 50*time.Millisecond || response.Basis != "recorded_harness_tool_interval" {
		t.Fatalf("source refusal was not preserved: %+v, %+v", request, response)
	}
	wrongCommand := refusal("/bin/bash -lc 'printf other'")
	if _, err := parseSource(wrongCommand, wrongCommand); err == nil {
		t.Fatal("a mismatched rejection command was accepted")
	}
	unknownFailure := "exec_command failed for `example\nProcess exited with code 0\nOutput:\nsample`: CreateProcess { message: \"unknown\" }"
	if _, err := parseSource(unknownFailure, unknownFailure); err == nil {
		t.Fatal("a process-creation failure with exit text in its command was accepted")
	}
	if _, err := parseSource(reply, wrongCommand); err == nil {
		t.Fatal("a mismatched native reply was accepted")
	}
	printedReply := "Process exited with code 0\nOutput:\n" + reply
	got, err = parseSource(printedReply, printedReply)
	if err != nil || len(got.Plan.Calls) != 2 || string(got.Plan.Calls[0].ResponseStdout) != reply {
		t.Fatalf("a shell reply printing an envelope changed classification: %+v, err %v", got, err)
	}
}
