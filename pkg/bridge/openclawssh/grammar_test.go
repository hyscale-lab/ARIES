package openclawssh

import (
	"strings"
	"testing"
)

func TestCanonicalRemoteCommandGrammar(t *testing.T) {
	t.Parallel()
	valid := [][]string{
		{remoteShell, "-c", "pwd"},
		{remoteShell, "-c", "printf '%s' \"$1\"", "shell-name", "a'b", ""},
		{remoteEnv, "LANG=C", "TERM=dumb", remoteShell, "-c", "cat"},
	}
	for _, tokens := range valid {
		encoded := encodeCanonicalTokens(tokens)
		decoded, err := decodeRemoteCommand(encoded)
		if err != nil {
			t.Fatalf("decodeRemoteCommand(%q) error = %v", encoded, err)
		}
		if strings.Join(decoded.argv, "\x00") != strings.Join(tokens, "\x00") {
			t.Fatalf("decoded argv = %#v, want %#v", decoded.argv, tokens)
		}
	}
}

func TestRemoteCommandRejectsAnythingOutsidePinnedGrammar(t *testing.T) {
	t.Parallel()
	tests := []string{
		"/bin/sh -c true",
		"'/bin/sh'  '-c' 'true'",
		"\"/bin/sh\" '-c' 'true'",
		"'/bin/bash' '-c' 'true'",
		"'/bin/sh' '-lc' 'true'",
		"'/bin/sh' '-c' ''",
		"'env' 'BAD-NAME=value' '/bin/sh' '-c' 'true'",
		"'env' 'DEEPSEEK_API_KEY=value' '/bin/sh' '-c' 'true'",
		"'env' 'SERVICE_TOKEN=value' '/bin/sh' '-c' 'true'",
		"'env' 'GITHUB_PAT=value' '/bin/sh' '-c' 'true'",
		"'env' 'CI_JOB_JWT=value' '/bin/sh' '-c' 'true'",
		"'env' 'TEST_VALUE=value' '/bin/sh' '-c' 'true'",
		"'env' 'ARIES_CONTROL=value' '/bin/sh' '-c' 'true'",
		"'env' 'LANG=C' 'LANG=en_US' '/bin/sh' '-c' 'true'",
		"'env' 'LANG' '/bin/sh' '-c' 'true'",
		"'env' 'LANG=C' 'printf' 'x'",
		"'/bin/sh' '-c' 'bad'junk",
		"'/bin/sh' '-c' 'unterminated",
		"'/bin/sh' '-c' 'nul\x00value'",
	}
	for _, encoded := range tests {
		if _, err := decodeRemoteCommand(encoded); err == nil {
			t.Errorf("decodeRemoteCommand(%q) unexpectedly succeeded", encoded)
		}
	}
}

func TestQuoteEscapeIsUniqueAndCanonical(t *testing.T) {
	t.Parallel()
	encoded := encodeCanonicalTokens([]string{remoteShell, "-c", "printf '%s'", "a'b"})
	if encoded != "'/bin/sh' '-c' 'printf '\\''%s'\\''' 'a'\\''b'" {
		t.Fatalf("encoded = %q", encoded)
	}
	if _, err := decodeRemoteCommand("'/bin/sh' '-c' 'ab''cd'"); err == nil {
		t.Fatal("noncanonical quote escape was accepted")
	}
}
