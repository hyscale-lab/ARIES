package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplayModeRequiresPrivateConfiguration(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"aries-bridge", "replay", filepath.Join(t.TempDir(), "missing.json")}
	if err := run(); err == nil || strings.Contains(err.Error(), "usage:") {
		t.Fatalf("replay mode did not read its private configuration: %v", err)
	}
}

func TestBridgeServerModeKeepsItsArguments(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"aries-bridge", filepath.Join(t.TempDir(), "missing.json")}
	if err := run(); err == nil || strings.Contains(err.Error(), "usage:") {
		t.Fatalf("bridge server mode rejected its existing arguments: %v", err)
	}
}
