package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hyscale-lab/aries/pkg/sandbox/docker/execproto"
)

func TestRunExecutesArgvDirectlyAndReportsStreams(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "helper.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("test sandbox forbids Unix listeners: %v", err)
		}
		t.Fatal(err)
	}
	defer listener.Close()
	wantArgs := append([]string(nil), os.Args...)
	os.Args = []string{"aries-exec-helper", socket, "/bin/sh", "-c", "cat; printf err >&2; exit 7"}
	defer func() { os.Args = wantArgs }()

	resultChannel := make(chan execproto.Result, 1)
	errorChannel := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			errorChannel <- acceptErr
			return
		}
		defer connection.Close()
		if protocolErr := execproto.ReadHello(connection); protocolErr != nil {
			errorChannel <- protocolErr
			return
		}
		if protocolErr := execproto.WriteInput(connection, []byte("stdin")); protocolErr != nil {
			errorChannel <- protocolErr
			return
		}
		result, protocolErr := execproto.ReadResult(connection, maxIO)
		if protocolErr != nil {
			errorChannel <- protocolErr
			return
		}
		resultChannel <- result
	}()
	if err := run(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	select {
	case err := <-errorChannel:
		t.Fatal(err)
	case result := <-resultChannel:
		if result.ExitCode != 7 || string(result.Stdout) != "stdin" || string(result.Stderr) != "err" {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestLimitedBufferConsumesButDoesNotRetainBeyondSharedLimit(t *testing.T) {
	limit := &sharedLimit{remaining: 3}
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	if _, err := stdout.Write([]byte("123")); err != nil {
		t.Fatal(err)
	}
	if written, err := stderr.Write([]byte("45")); err != nil || written != 2 || !limit.exceeded || stderr.buffer.Len() != 0 {
		t.Fatal("shared output limit was not enforced")
	}
}

func TestTrustedFileOperationsRemoveWithoutFollowingAndVerifyExactAlias(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "workspace")
	alias := filepath.Join(root, "work")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if err := runTrustedFileOperation([]string{"--verify-alias", alias, target}); err != nil {
		t.Fatal(err)
	}
	if err := runTrustedFileOperation([]string{"--verify-workspace", alias, target}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runTrustedFileOperation([]string{"--verify-alias", alias, other}); err == nil {
		t.Fatal("wrong alias target was accepted")
	}
	reverse := filepath.Join(root, "reverse")
	if err := os.Symlink(target, reverse); err != nil {
		t.Fatal(err)
	}
	if err := runTrustedFileOperation([]string{"--verify-workspace", target, reverse}); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(root, "server")
	if err := os.WriteFile(file, []byte("server"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runTrustedFileOperation([]string{"--remove-file", file}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed file remains: %v", err)
	}
	if err := runTrustedFileOperation([]string{"--remove-file", file}); err != nil {
		t.Fatalf("repeated remove error = %v", err)
	}
	if err := runTrustedFileOperation([]string{"--remove-file", target}); err == nil {
		t.Fatal("directory removal was accepted")
	}
}

func TestTrustedFileOperationsRejectUnsafeShapes(t *testing.T) {
	for _, args := range [][]string{
		{"--remove-file", "/"},
		{"--remove-file", "relative"},
		{"--verify-alias", "/one"},
		{"--verify-workspace", "/one"},
		{"--unknown", "/one"},
	} {
		if err := runTrustedFileOperation(args); err == nil {
			t.Errorf("operation %#v unexpectedly succeeded", args)
		}
	}
}

func TestTrustedWorkspaceRecoveryReconcilesEveryExactPrepareState(t *testing.T) {
	ownerToken := bytes.Repeat([]byte{0x5a}, workspaceOwnerTokenBytes)
	for _, state := range []string{"unchanged", "roots-created", "renamed", "aliased", "reverse-aliased"} {
		t.Run(state, func(t *testing.T) {
			base := t.TempDir()
			workdir := filepath.Join(base, "work")
			root := filepath.Join(base, "openclaw")
			runtimeRoot := filepath.Join(root, "runtime")
			workspace := filepath.Join(runtimeRoot, "workspace")
			if err := os.Mkdir(workdir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workdir, "state"), []byte("preserved"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeWorkspaceOwnerMarker(root, ownerToken); err != nil {
				t.Fatal(err)
			}
			if state != "unchanged" {
				if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if state == "renamed" || state == "aliased" {
				if err := os.Rename(workdir, workspace); err != nil {
					t.Fatal(err)
				}
			}
			if state == "aliased" {
				if err := os.Symlink(workspace, workdir); err != nil {
					t.Fatal(err)
				}
			}
			if state == "reverse-aliased" {
				if err := os.Symlink(workdir, workspace); err != nil {
					t.Fatal(err)
				}
			}
			if err := runTrustedFileOperationWithInput([]string{"--recover-workspace", workdir, root, "runtime"}, bytes.NewReader(ownerToken)); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(workdir)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("restored workdir = %v, %v", info, err)
			}
			content, err := os.ReadFile(filepath.Join(workdir, "state"))
			if err != nil || string(content) != "preserved" {
				t.Fatalf("state = %q, %v", content, err)
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace root remains: %v", err)
			}
		})
	}
}

func TestTrustedWorkspaceRecoveryLeavesUnmarkedPreexistingEmptyRootUntouched(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	root := filepath.Join(base, "openclaw")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ownerToken := bytes.Repeat([]byte{0x5a}, workspaceOwnerTokenBytes)
	if err := runTrustedFileOperationWithInput([]string{"--recover-workspace", workdir, root, "runtime"}, bytes.NewReader(ownerToken)); err == nil {
		t.Fatal("unmarked pre-existing empty workspace root authorized recovery")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("pre-existing empty workspace root changed: %v, %v", info, err)
	}
}

func TestTrustedWorkspaceRecoveryRejectsForeignAndSymlinkedStates(t *testing.T) {
	ownerToken := bytes.Repeat([]byte{0x5a}, workspaceOwnerTokenBytes)
	t.Run("foreign runtime entry", func(t *testing.T) {
		base := t.TempDir()
		workdir := filepath.Join(base, "work")
		root := filepath.Join(base, "root")
		if err := os.Mkdir(workdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeWorkspaceOwnerMarker(root, ownerToken); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "runtime"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "runtime", "foreign"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runTrustedFileOperationWithInput([]string{"--recover-workspace", workdir, root, "runtime"}, bytes.NewReader(ownerToken)); err == nil {
			t.Fatal("foreign runtime state was removed")
		}
		if _, err := os.Lstat(filepath.Join(root, "runtime", "foreign")); err != nil {
			t.Fatalf("foreign runtime entry changed: %v", err)
		}
	})
	t.Run("symlinked root ancestor", func(t *testing.T) {
		base := t.TempDir()
		realParent := filepath.Join(base, "real")
		aliasParent := filepath.Join(base, "alias")
		workdir := filepath.Join(base, "work")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(workdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, aliasParent); err != nil {
			t.Fatal(err)
		}
		if err := runTrustedFileOperationWithInput([]string{"--recover-workspace", workdir, filepath.Join(aliasParent, "root"), "runtime"}, bytes.NewReader(ownerToken)); err == nil {
			t.Fatal("symlinked workspace ancestor was accepted")
		}
	})
}

func TestTrustedWorkspaceRecoveryRejectsMarkerMismatchAndMode(t *testing.T) {
	for _, test := range []struct {
		name       string
		markerMode os.FileMode
		markerByte byte
	}{
		{name: "mismatch", markerMode: 0o600, markerByte: 0x33},
		{name: "mode", markerMode: 0o644, markerByte: 0x5a},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			workdir := filepath.Join(base, "work")
			root := filepath.Join(base, "root")
			if err := os.Mkdir(workdir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, workspaceOwnerMarker)
			if err := os.WriteFile(marker, bytes.Repeat([]byte{test.markerByte}, workspaceOwnerTokenBytes), test.markerMode); err != nil {
				t.Fatal(err)
			}
			ownerToken := bytes.Repeat([]byte{0x5a}, workspaceOwnerTokenBytes)
			if err := runTrustedFileOperationWithInput([]string{"--recover-workspace", workdir, root, "runtime"}, bytes.NewReader(ownerToken)); err == nil {
				t.Fatal("invalid owner marker authorized recovery")
			}
			if _, err := os.Lstat(root); err != nil {
				t.Fatalf("invalid owner root changed: %v", err)
			}
		})
	}

	t.Run("symlink", func(t *testing.T) {
		base := t.TempDir()
		workdir := filepath.Join(base, "work")
		root := filepath.Join(base, "root")
		outside := filepath.Join(base, "outside")
		if err := os.Mkdir(workdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		ownerToken := bytes.Repeat([]byte{0x5a}, workspaceOwnerTokenBytes)
		if err := os.WriteFile(outside, ownerToken, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, workspaceOwnerMarker)); err != nil {
			t.Fatal(err)
		}
		if err := runTrustedFileOperationWithInput([]string{"--recover-workspace", workdir, root, "runtime"}, bytes.NewReader(ownerToken)); err == nil {
			t.Fatal("symlinked owner marker authorized recovery")
		}
		content, err := os.ReadFile(outside)
		if err != nil || !bytes.Equal(content, ownerToken) {
			t.Fatalf("symlink target changed: %x, %v", content, err)
		}
	})
}
