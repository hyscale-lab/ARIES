package openclawssh

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestVirtualWorkspaceProtocolConstants(t *testing.T) {
	if virtualRuntimeRoot != "/aries/openclaw/openclaw-ssh-shared-8198076c" {
		t.Fatalf("virtualRuntimeRoot = %q", virtualRuntimeRoot)
	}
	if virtualWorkspace != virtualRuntimeRoot+"/workspace" {
		t.Fatalf("virtualWorkspace = %q", virtualWorkspace)
	}
}

func TestPinnedTransportControlLiterals(t *testing.T) {
	tests := map[string]struct {
		got, want string
	}{
		"probe script": {
			got: runtimeProbeScript, want: `if [ -d "$1" ]; then printf "1\n"; else printf "0\n"; fi`,
		},
		"probe sentinel": {got: runtimeProbeSentinel, want: "openclaw-sandbox-check"},
		"workspace clear script": {
			got: workspaceClearScript, want: `mkdir -p -- "$1" && find "$1" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +`,
		},
		"workspace clear sentinel": {got: workspaceClearLabel, want: "openclaw-sandbox-clear"},
		"runtime remove script":    {got: runtimeRemoveScript, want: `rm -rf -- "$1"`},
		"runtime remove sentinel":  {got: runtimeRemoveLabel, want: "openclaw-sandbox-remove"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("pinned literal changed byte-for-byte:\n got %q\nwant %q", test.got, test.want)
			}
		})
	}
}

func TestRuntimeProbeSubstitutesOnlyItsExactFinalRootArgument(t *testing.T) {
	want := []string{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, "/app/personal-site"}
	plan, err := prepareRemoteCommand(remoteCommand{argv: []string{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, virtualRuntimeRoot}}, "/app/personal-site")
	if err != nil {
		t.Fatal(err)
	}
	if got := append([]string{plan.command.Path}, plan.command.Args...); !reflect.DeepEqual(got, want) {
		t.Fatalf("probe argv = %#v, want %#v", got, want)
	}
	if plan.command.Dir != "/app/personal-site" || plan.suppressed {
		t.Fatalf("probe plan = %#v", plan)
	}

	nearMatches := [][]string{
		{remoteShell, "-c", runtimeProbeScript + " ", runtimeProbeSentinel, virtualRuntimeRoot},
		{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel + "-near", virtualRuntimeRoot},
		{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, virtualWorkspace},
		{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, virtualRuntimeRoot + "/"},
		{remoteShell, "-c", runtimeProbeScript, virtualRuntimeRoot, runtimeProbeSentinel},
		{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, virtualRuntimeRoot, "extra"},
	}
	for _, argv := range nearMatches {
		plan, err := prepareRemoteCommand(remoteCommand{argv: argv}, "/workspace")
		if err != nil {
			continue
		}
		got := append([]string{plan.command.Path}, plan.command.Args...)
		if reflect.DeepEqual(got, []string{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, "/workspace"}) {
			t.Fatalf("near-match probe was substituted: %#v", argv)
		}
	}
}

func TestTransportCleanupControlsAreClassifiedBeforeMapping(t *testing.T) {
	controls := [][]string{
		{remoteShell, "-c", workspaceClearScript, workspaceClearLabel, virtualWorkspace},
		{remoteShell, "-c", runtimeRemoveScript, runtimeRemoveLabel, virtualRuntimeRoot},
	}
	for _, argv := range controls {
		plan, err := prepareRemoteCommand(remoteCommand{argv: argv}, "/workspace")
		if err != nil {
			t.Fatal(err)
		}
		if !plan.suppressed {
			t.Fatalf("transport cleanup was not suppressed: %#v", argv)
		}
	}

	near := []string{remoteShell, "-c", workspaceClearScript, workspaceClearLabel + "-agent", virtualWorkspace}
	plan, err := prepareRemoteCommand(remoteCommand{argv: near}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if plan.suppressed {
		t.Fatal("near-match transport cleanup was suppressed")
	}
}

func TestTransportCleanupNearMatchesAreNeverSuppressed(t *testing.T) {
	controls := []struct {
		name, script, sentinel, path, alternatePath string
	}{
		{name: "workspace clear", script: workspaceClearScript, sentinel: workspaceClearLabel, path: virtualWorkspace, alternatePath: virtualRuntimeRoot},
		{name: "runtime remove", script: runtimeRemoveScript, sentinel: runtimeRemoveLabel, path: virtualRuntimeRoot, alternatePath: virtualWorkspace},
	}
	for _, control := range controls {
		t.Run(control.name, func(t *testing.T) {
			nearMatches := map[string][]string{
				"wrong script":   {remoteShell, "-c", control.script + " ", control.sentinel, control.path},
				"wrong sentinel": {remoteShell, "-c", control.script, control.sentinel + "-near", control.path},
				"missing path":   {remoteShell, "-c", control.script, control.sentinel},
				"extra argument": {remoteShell, "-c", control.script, control.sentinel, control.path, "extra"},
				"wrong order":    {remoteShell, "-c", control.script, control.path, control.sentinel},
				"wrong path":     {remoteShell, "-c", control.script, control.sentinel, control.alternatePath},
				"path suffix":    {remoteShell, "-c", control.script, control.sentinel, control.path + "/"},
			}
			for name, argv := range nearMatches {
				t.Run(name, func(t *testing.T) {
					plan, err := prepareRemoteCommand(remoteCommand{argv: argv}, "/workspace")
					if err == nil && plan.suppressed {
						t.Fatalf("near-match control was suppressed: %#v", argv)
					}
				})
			}
		})
	}
}

func TestGeneratedWorkspaceCommandMapsHomePrefixAndBoundedReferences(t *testing.T) {
	remainder := "cd " + virtualWorkspace + " && cat '" + virtualWorkspace + "/input' > \"" + virtualWorkspace + "/output\""
	remote := remoteCommand{argv: generatedArgv(remainder)}
	plan, err := prepareRemoteCommand(remote, "/app/personal-site")
	if err != nil {
		t.Fatal(err)
	}
	wantScript := "cd /app/personal-site && cat '/app/personal-site/input' > \"/app/personal-site/output\""
	if plan.command.Args[1] != wantScript || plan.command.Dir != "/app/personal-site" {
		t.Fatalf("generated command = %#v", plan.command)
	}
	if plan.command.Env["HOME"] != "/app/personal-site" || plan.workspaceHome != "/app/personal-site" {
		t.Fatalf("generated HOME = %#v / %q", plan.command.Env, plan.workspaceHome)
	}
	if plan.command.Env["PATH"] != "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin" || plan.command.Env["LANG"] != "C.UTF-8" || plan.command.Env["OPENCLAW_SHELL"] != "exec" {
		t.Fatalf("unrelated generated environment changed: %#v", plan.command.Env)
	}
	if strings.Contains(plan.command.Args[1], virtualRuntimeRoot) || plan.encoded == encodeCanonicalTokens(remote.argv) {
		t.Fatalf("translation did not change only executed evidence: %#v", plan)
	}
}

func TestGeneratedWorkspaceHomeShapeFailsClosed(t *testing.T) {
	tests := map[string]func([]string) []string{
		"missing":     func(argv []string) []string { return append(argv[:2], argv[3:]...) },
		"wrong value": func(argv []string) []string { argv[2] = "HOME=/near"; return argv },
		"wrong case":  func(argv []string) []string { argv[2] = "Home=" + virtualWorkspace; return argv },
		"duplicate": func(argv []string) []string {
			return append(argv[:3], append([]string{"HOME=" + virtualWorkspace}, argv[3:]...)...)
		},
		"reordered":      func(argv []string) []string { argv[1], argv[2] = argv[2], argv[1]; return argv },
		"after sentinel": func(argv []string) []string { argv[2], argv[4] = argv[4], argv[2]; return argv },
		"extra":          func(argv []string) []string { return append(argv[:4], append([]string{"EXTRA=1"}, argv[4:]...)...) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			argv := mutate(generatedArgv("true"))
			if _, err := prepareRemoteCommand(remoteCommand{argv: argv}, "/workspace"); err == nil {
				t.Fatalf("generated argv unexpectedly accepted: %#v", argv)
			}
		})
	}

	ordinary := remoteCommand{argv: []string{remoteEnv, "HOME=" + virtualWorkspace, remoteShell, "-c", "true"}}
	if _, err := prepareRemoteCommand(ordinary, "/workspace"); err == nil {
		t.Fatal("ordinary command leaked exact virtual HOME")
	}
	unrelated := remoteCommand{argv: []string{remoteEnv, "HOME=/ordinary", remoteShell, "-c", "true"}}
	plan, err := prepareRemoteCommand(unrelated, "/workspace")
	if err != nil || plan.command.Env["HOME"] != "/ordinary" {
		t.Fatalf("ordinary HOME changed: %#v, %v", plan, err)
	}
}

func TestTranslateVirtualWorkspaceUsesExactBoundariesAndRootSafeJoin(t *testing.T) {
	delimiters := []byte(" \t\n\r'\"`(){}[];|&<>=:,")
	for _, delimiter := range delimiters {
		input := string(delimiter) + virtualWorkspace + string(delimiter)
		got, err := translateVirtualWorkspace(input, "/work")
		if err != nil || got != string(delimiter)+"/work"+string(delimiter) {
			t.Fatalf("delimiter %q = %q, %v", delimiter, got, err)
		}
	}

	tests := []struct {
		name, input, workdir, want string
	}{
		{name: "standalone", input: virtualWorkspace, workdir: "/work", want: "/work"},
		{name: "descendant", input: "cat " + virtualWorkspace + "/a", workdir: "/work", want: "cat /work/a"},
		{name: "root standalone", input: "'" + virtualWorkspace + "'", workdir: "/", want: "'/'"},
		{name: "root descendant", input: ">" + virtualWorkspace + "/a", workdir: "/", want: ">/a"},
		{name: "multiple nested", input: "printf '%s' \"" + virtualWorkspace + "/a\"; cat `echo " + virtualWorkspace + "`", workdir: "/safe", want: "printf '%s' \"/safe/a\"; cat `echo /safe`"},
		{name: "heredoc", input: "cat <<'EOF'\n" + virtualWorkspace + "/body\nEOF", workdir: "/safe", want: "cat <<'EOF'\n/safe/body\nEOF"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := translateVirtualWorkspace(test.input, test.workdir)
			if err != nil || got != test.want || strings.Contains(got, "//") {
				t.Fatalf("translate = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestTranslateVirtualWorkspaceRejectsNearAmbiguousAndRuntimeRoot(t *testing.T) {
	for _, input := range []string{
		"prefix" + virtualWorkspace,
		virtualWorkspace + "suffix",
		"/near" + virtualWorkspace + "/child",
		virtualRuntimeRoot,
		"cat " + virtualRuntimeRoot + "/other",
		"cat " + virtualWorkspace + "-near",
	} {
		if got, err := translateVirtualWorkspace(input, "/work"); err == nil || got != "" {
			t.Fatalf("ambiguous namespace %q = %q, %v", input, got, err)
		}
	}
}

func TestTranslateVirtualWorkspaceRejectsRepeatedSlashDescendantsWithoutNormalization(t *testing.T) {
	for _, workdir := range []string{"/work", "/"} {
		for name, input := range map[string]string{
			"plain":    virtualWorkspace + "//child",
			"quoted":   "cat '" + virtualWorkspace + "//child'",
			"multiple": "cat " + virtualWorkspace + "/valid " + virtualWorkspace + "//child",
			"redirect": "printf x >\"" + virtualWorkspace + "//child\"",
		} {
			t.Run(workdir+"/"+name, func(t *testing.T) {
				if got, err := translateVirtualWorkspace(input, workdir); err == nil || got != "" {
					t.Fatalf("repeated-slash descendant normalized or accepted: got %q, err %v", got, err)
				}
			})
		}
	}
}

func TestGeneratedOrdinaryMutationMapsButExactTransportCleanupDoesNot(t *testing.T) {
	mutation, err := prepareRemoteCommand(remoteCommand{argv: generatedArgv("rm -f " + virtualWorkspace + "/agent-file")}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if mutation.suppressed || mutation.command.Args[1] != "rm -f /workspace/agent-file" {
		t.Fatalf("ordinary mutation = %#v", mutation)
	}
	cleanup, err := prepareRemoteCommand(remoteCommand{argv: []string{remoteShell, "-c", workspaceClearScript, workspaceClearLabel, virtualWorkspace}}, "/workspace")
	if err != nil || !cleanup.suppressed {
		t.Fatalf("transport cleanup = %#v, %v", cleanup, err)
	}
}

func TestVirtualizedExecutionKeepsWireEvidenceAndRecordsExecutedState(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	sandbox := &contractSandbox{acceptTools: true}
	session := &bridgeSession{sandbox: sandbox, audit: newAuditWriter(structured, raw)}
	remote := remoteCommand{argv: generatedArgv("cd " + virtualWorkspace + " && cat " + virtualWorkspace + "/input >" + virtualWorkspace + "/output")}
	wire := encodeCanonicalTokens(remote.argv)
	prepared, err := prepareRemoteCommand(remote, sandbox.Workdir())
	if err != nil {
		t.Fatal(err)
	}
	payload := ssh.Marshal(struct{ Command string }{wire})
	if exit := session.execute(context.Background(), &stubSSHChannel{}, prepared, requestAudit{
		requestType: "exec", wantReply: true, payload: payload, remoteCommand: wire,
	}); exit != 0 {
		t.Fatalf("virtualized exit = %d", exit)
	}
	if err := session.closeAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := sandbox.snapshot()
	if len(commands) != 1 || commands[0].Args[1] != "cd /workspace && cat /workspace/input >/workspace/output" || commands[0].Env["HOME"] != "/workspace" {
		t.Fatalf("executed commands = %#v", commands)
	}
	records := decodeAuditLines(t, structuredBytes.Bytes())
	if len(records) != 1 || records[0]["workspace_home"] != "/workspace" || records[0]["command"] != commands[0].Args[1] {
		t.Fatalf("structured record = %#v", records)
	}
	if records[0]["command_hash"] != commandHash(prepared.encoded) || records[0]["command_hash"] == commandHash(wire) {
		t.Fatalf("structured command hash = %#v", records[0]["command_hash"])
	}
	envNames, ok := records[0]["env_names"].([]any)
	if !ok || !containsAny(envNames, "HOME") {
		t.Fatalf("structured env names = %#v", records[0]["env_names"])
	}
	rawRecords := decodeRawAuditRecords(t, rawBytes.Bytes())
	if len(rawRecords) != 1 || rawRecords[0]["wire_command"] != wire || !bytes.Equal(unescapeRawValue(t, rawRecords[0]["payload"]), payload) {
		t.Fatalf("raw record = %#v", rawRecords)
	}
}

func TestSuppressedTransportCleanupNeverExecutesSandbox(t *testing.T) {
	structured, _ := memoryAuditFile()
	raw, _ := memoryAuditFile()
	sandbox := &contractSandbox{acceptTools: true}
	session := &bridgeSession{sandbox: sandbox, audit: newAuditWriter(structured, raw)}
	remote := remoteCommand{argv: []string{remoteShell, "-c", workspaceClearScript, workspaceClearLabel, virtualWorkspace}}
	prepared, err := prepareRemoteCommand(remote, sandbox.Workdir())
	if err != nil {
		t.Fatal(err)
	}
	wire := encodeCanonicalTokens(remote.argv)
	if exit := session.execute(context.Background(), &stubSSHChannel{}, prepared, requestAudit{requestType: "exec", wantReply: true, remoteCommand: wire}); exit != 0 {
		t.Fatalf("suppressed cleanup exit = %d", exit)
	}
	if err := session.closeAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commands := sandbox.snapshot(); len(commands) != 0 {
		t.Fatalf("transport cleanup executed sandbox: %#v", commands)
	}
}

func containsAny(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func generatedArgv(remainder string) []string {
	return []string{
		remoteEnv,
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
		"HOME=" + virtualWorkspace,
		"LANG=C.UTF-8",
		"OPENCLAW_SHELL=exec",
		remoteShell,
		"-c",
		generatedWorkspacePrefix + remainder,
	}
}
