package apptainer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stand-in slirp4netns reports ready on descriptor 3 and runs until its exit
// pipe, descriptor 4, is closed, as slirp4netns does with --exit-fd.
const fakeSlirp = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_SLIRP_ARGS"
printf 1 >&3
cat <&4 > /dev/null
`

func TestSlirpExitsWhenItsExitPipeCloses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slirp4netns")
	if err := os.WriteFile(path, []byte(fakeSlirp), 0o700); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args")
	t.Setenv("FAKE_SLIRP_ARGS", argsFile)

	network, exit, ready, err := startSlirp(path, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stand-in never reported ready")
	}
	done := make(chan error, 1)
	go func() { done <- network.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("slirp4netns exited while its exit pipe was open: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := exit.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slirp4netns kept running after its exit pipe closed")
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--exit-fd=4\n") || !strings.Contains(string(args), "--ready-fd=3\n") {
		t.Fatalf("args=%q", args)
	}
}
