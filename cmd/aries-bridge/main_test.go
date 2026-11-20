package main

import (
	"errors"
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

func TestBridgeUsesNativeFlags(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		name     string
		args     []string
		wantErr  error
		wantText string
	}{
		{name: "server terminator", args: []string{"--", "-missing.json"}, wantErr: os.ErrNotExist},
		{name: "replay terminator", args: []string{"--", "replay", "missing.json"}, wantErr: os.ErrNotExist},
		{name: "stop at positional", args: []string{"replay", "--unknown"}, wantErr: os.ErrNotExist},
		{name: "unknown flag", args: []string{"--unknown"}, wantText: "flag provided but not defined"},
		{name: "empty", wantText: "usage:"},
		{name: "extra argument", args: []string{"config.json", "extra"}, wantText: "usage:"},
		{name: "replay extra argument", args: []string{"replay", "config.json", "extra"}, wantText: "usage:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			os.Args = append([]string{"aries-bridge"}, test.args...)
			err := run()
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v, want %v", err, test.wantErr)
			}
			if test.wantText != "" && (err == nil || !strings.Contains(err.Error(), test.wantText)) {
				t.Fatalf("error=%v, want %q", err, test.wantText)
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
