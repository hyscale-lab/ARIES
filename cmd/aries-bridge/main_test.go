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
	const privateMode = 0o600
	const publicMode = 0o644
	root := t.TempDir()
	private := filepath.Join(root, "private.json")
	public := filepath.Join(root, "public.json")
	link := filepath.Join(root, "link.json")
	for path, mode := range map[string]os.FileMode{private: privateMode, public: publicMode} {
		if err := os.WriteFile(path, []byte("{}"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, path string }{
		{name: "public permissions", path: public},
		{name: "symlink", path: link},
	} {
		t.Run(test.name, func(t *testing.T) {
			os.Args = []string{"aries-bridge", "replay", test.path}
			if err := run(); err == nil || !strings.Contains(err.Error(), "invalid private replay caller file") {
				t.Fatalf("unsafe replay configuration passed private-file validation: %v", err)
			}
		})
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
