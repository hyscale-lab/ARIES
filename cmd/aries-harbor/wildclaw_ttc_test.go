package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestTTCReplayToolPreservesScriptAndExit(t *testing.T) {
	proxy := filepath.Join(t.TempDir(), "ttc")
	source := `#!/bin/sh
[ "$1" = exec ] && [ "$2" = --workdir ] && [ "$3" = /tmp_workspace ] &&
[ "$4" = fixture@main ] && [ "$5" = /bin/bash ] && [ "$6" = -c ] || exit 91
exec "$5" "$6" "$7"
`
	if err := os.WriteFile(proxy, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := ttcReplayTool(context.Background(), proxy, "fixture@main", replay.Call{
		Operation: "Exec", Workdir: "/tmp_workspace", Script: "printf '%s\\n' 'a b'; exit 128",
	})
	if err != nil || result.ExitCode != 128 || result.Stdout != "a b\n" {
		t.Fatalf("rewritten command or exit: %+v, %v", result, err)
	}
}
