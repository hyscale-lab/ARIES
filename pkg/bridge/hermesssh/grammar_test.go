package hermesssh

import (
	"errors"
	"strings"
	"testing"
)

// capturedLoginPayload and capturedAgentPayload are verbatim `exec` payloads
// recorded from Hermes v2026.5.29.2 (tools/environments/ssh.py) driving a
// logging SSH server. They pin the real wire shape, including the embedded
// newlines and '"'"' quote escapes that make Hermes incompatible with
// OpenClaw's canonical single-token grammar.
const (
	capturedLoginPayload = "bash -l -c 'export -p > /tmp/hermes-snap-5db98cc5d0bd.sh\ndeclare -f | grep -vE '\"'\"'^_[^_]'\"'\"' >> /tmp/hermes-snap-5db98cc5d0bd.sh\nalias -p >> /tmp/hermes-snap-5db98cc5d0bd.sh\necho '\"'\"'shopt -s expand_aliases'\"'\"' >> /tmp/hermes-snap-5db98cc5d0bd.sh\necho '\"'\"'set +e'\"'\"' >> /tmp/hermes-snap-5db98cc5d0bd.sh\necho '\"'\"'set +u'\"'\"' >> /tmp/hermes-snap-5db98cc5d0bd.sh\nbuiltin cd /tmp 2>/dev/null || true\npwd -P > /tmp/hermes-cwd-5db98cc5d0bd.txt 2>/dev/null || true\nprintf '\"'\"'\\n__HERMES_CWD_5db98cc5d0bd__%s__HERMES_CWD_5db98cc5d0bd__\\n'\"'\"' \"$(pwd -P)\"\n'"
	capturedAgentPayload = "bash -c 'source /tmp/hermes-snap-5db98cc5d0bd.sh >/dev/null 2>&1 || true\nbuiltin cd -- /tmp || exit 126\neval '\"'\"'echo hello-from-agent && pwd'\"'\"'\n__hermes_ec=$?\nexport -p > /tmp/hermes-snap-5db98cc5d0bd.sh 2>/dev/null || true\npwd -P > /tmp/hermes-cwd-5db98cc5d0bd.txt 2>/dev/null || true\nprintf '\"'\"'\\n__HERMES_CWD_5db98cc5d0bd__%s__HERMES_CWD_5db98cc5d0bd__\\n'\"'\"' \"$(pwd -P)\"\nexit $__hermes_ec'"
)

func TestDecodeAcceptsCapturedHermesPayloads(t *testing.T) {
	login, err := decodeRemoteCommand(capturedLoginPayload)
	if err != nil {
		t.Fatalf("captured login payload rejected: %v", err)
	}
	if !login.login || login.kind != kindAgent {
		t.Fatalf("login payload decoded as login=%v kind=%q", login.login, login.kind)
	}
	if !strings.Contains(login.script, "shopt -s expand_aliases") || !strings.Contains(login.script, "\n") {
		t.Fatalf("login script lost content: %q", login.script)
	}
	if !strings.Contains(login.script, "grep -vE '^_[^_]'") {
		t.Fatalf("login script did not unescape the embedded quotes: %q", login.script)
	}

	agent, err := decodeRemoteCommand(capturedAgentPayload)
	if err != nil {
		t.Fatalf("captured agent payload rejected: %v", err)
	}
	if agent.login || agent.kind != kindAgent {
		t.Fatalf("agent payload decoded as login=%v kind=%q", agent.login, agent.kind)
	}
	if !strings.Contains(agent.script, "eval 'echo hello-from-agent && pwd'") {
		t.Fatalf("agent script did not unescape the embedded command: %q", agent.script)
	}
}

func TestDecodeAcceptsBootstrapProbes(t *testing.T) {
	for _, payload := range []string{connectionProbePayload, remoteHomePayload} {
		command, err := decodeRemoteCommand(payload)
		if err != nil {
			t.Fatalf("bootstrap payload %q rejected: %v", payload, err)
		}
		if command.kind != kindBootstrap {
			t.Fatalf("bootstrap payload %q decoded as kind %q", payload, command.kind)
		}
	}
}

// The file-sync payloads are the ones ARIES refuses on purpose. They are
// reported separately from malformed input so the evidence record distinguishes
// policy from a protocol violation.
func TestDecodeDeniesCapturedFileSyncPayloads(t *testing.T) {
	captured := []string{
		"mkdir -p /home/colin/.hermes /home/colin/.hermes/skills /home/colin/.hermes/credentials /home/colin/.hermes/cache",
		"mkdir -p /home/colin/.hermes/skills/demo",
		"tar xf - --no-overwrite-dir -C /home/colin/.hermes",
		"tar cf - -C / home/colin/.hermes",
		"rm -f /home/colin/.hermes/skills/demo/skill.md",
		"scp -t /home/colin/.hermes/skills/demo/skill.md",
	}
	for _, payload := range captured {
		_, err := decodeRemoteCommand(payload)
		if !errors.Is(err, errSyncDenied) {
			t.Fatalf("payload %q returned %v, want errSyncDenied", payload, err)
		}
	}
}

func TestDecodeRejectsEverythingElse(t *testing.T) {
	rejected := map[string]string{
		"empty":              "",
		"nul":                "bash -c 'ls\x00'",
		"other shell":        "/bin/sh -c 'ls'",
		"openclaw env shape": "'env' 'HOME=/x' '/bin/sh' '-c' 'ls'",
		"no -c":              "bash 'ls'",
		"empty script":       "bash -c ''",
		"trailing token":     "bash -c 'ls' extra",
		"unterminated":       "bash -c 'ls",
		"non canonical":      `bash -c "ls"`,
		"bare unsafe":        "bash -c ls|whoami",
		"login without -c":   "bash -l 'ls'",
	}
	for name, payload := range rejected {
		if _, err := decodeRemoteCommand(payload); err == nil {
			t.Fatalf("%s: payload %q was accepted", name, payload)
		}
	}
}

// A bare token is only legal when shlex.quote would itself have left it bare;
// anything else must round-trip through the quoted form or be refused.
func TestDecodeRequiresCanonicalQuoting(t *testing.T) {
	if command, err := decodeRemoteCommand("bash -c ls"); err != nil || command.script != "ls" {
		t.Fatalf("bare safe token: got %+v err %v", command, err)
	}
	if _, err := decodeRemoteCommand("bash -c 'ls'"); err == nil {
		t.Fatal("redundantly quoted safe token was accepted")
	}
}

func TestShlexQuoteMatchesPython(t *testing.T) {
	cases := map[string]string{
		"":                  "''",
		"ls":                "ls",
		"a/b_c-d.e:f,g@h%i": "a/b_c-d.e:f,g@h%i",
		"ls -l":             "'ls -l'",
		"it's":              `'it'"'"'s'`,
		"a\nb":              "'a\nb'",
		"$HOME":             "'$HOME'",
	}
	for input, want := range cases {
		if got := shlexQuote(input); got != want {
			t.Fatalf("shlexQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDecodeRoundTripsArbitraryScripts(t *testing.T) {
	scripts := []string{
		"echo hello",
		"grep -vE '^_[^_]' file",
		"printf 'a\\nb'\nexit $?",
		"echo \"double\" && echo 'single'",
	}
	for _, script := range scripts {
		for _, prefix := range []string{"bash -c ", "bash -l -c "} {
			payload := prefix + shlexQuote(script)
			command, err := decodeRemoteCommand(payload)
			if err != nil {
				t.Fatalf("payload %q rejected: %v", payload, err)
			}
			if command.script != script {
				t.Fatalf("round trip lost data: got %q want %q", command.script, script)
			}
		}
	}
}
