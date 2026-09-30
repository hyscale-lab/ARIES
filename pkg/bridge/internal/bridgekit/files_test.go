package bridgekit

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWriteExclusiveExactModesUnderRestrictiveUmask(t *testing.T) {
	const childMarker = "ARIES_TEST_RESTRICTIVE_UMASK_CHILD"
	if os.Getenv(childMarker) == "1" {
		syscall.Umask(0o077)
		source := os.Getenv("ARIES_TEST_EXECUTABLE_SOURCE")
		executable := os.Getenv("ARIES_TEST_EXECUTABLE_DESTINATION")
		private := os.Getenv("ARIES_TEST_PRIVATE_DESTINATION")
		content := []byte("#!/bin/sh\nexit 0\n")
		if err := StageExecutable(source, executable); err != nil {
			t.Fatal(err)
		}
		if err := WriteExclusivePrivate(private, content); err != nil {
			t.Fatal(err)
		}
		for path, wantMode := range map[string]os.FileMode{executable: 0o555, private: 0o600} {
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("%s content = %q", filepath.Base(path), got)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != wantMode {
				t.Fatalf("%s mode = %04o, want %04o", filepath.Base(path), info.Mode().Perm(), wantMode)
			}
		}
		return
	}

	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	content := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(source, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestWriteExclusiveExactModesUnderRestrictiveUmask$")
	command.Env = append(os.Environ(),
		childMarker+"=1",
		"ARIES_TEST_EXECUTABLE_SOURCE="+source,
		"ARIES_TEST_EXECUTABLE_DESTINATION="+filepath.Join(directory, "staged"),
		"ARIES_TEST_PRIVATE_DESTINATION="+filepath.Join(directory, "private"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restrictive-umask subprocess failed: %v\n%s", err, output)
	}
}

func TestWriteExclusiveUsesPrivateOrderedDescriptorTransition(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "staged")
	content := []byte("complete executable")
	var events []string
	var openFlags int
	var createMode os.FileMode
	dataSynced := false
	syncCalls := 0
	operations := exclusiveWriteOperations{
		open: func(name string, flags int, mode os.FileMode) (exclusiveWriteFile, error) {
			events = append(events, "open")
			openFlags = flags
			createMode = mode
			file, err := os.OpenFile(name, flags, mode)
			if err != nil {
				return nil, err
			}
			return &testExclusiveWriteFile{
				write: func(got []byte) (int, error) {
					events = append(events, "write")
					requireFileMode(t, path, 0o600)
					return file.Write(got)
				},
				sync: func() error {
					syncCalls++
					if syncCalls == 1 {
						events = append(events, "data sync")
						requireFileMode(t, path, 0o600)
						dataSynced = true
					} else {
						events = append(events, "metadata sync")
						requireFileMode(t, path, 0o555)
					}
					return file.Sync()
				},
				chmod: func(mode os.FileMode) error {
					events = append(events, "chmod")
					if !dataSynced {
						t.Fatal("chmod ran before the content sync")
					}
					got, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, content) {
						t.Fatalf("content at chmod = %q", got)
					}
					return file.Chmod(mode)
				},
				close: func() error {
					events = append(events, "close")
					return file.Close()
				},
			}, nil
		},
		remove: os.Remove,
	}
	if err := writeExclusiveWithOperations(path, content, 0o555, operations); err != nil {
		t.Fatal(err)
	}
	if openFlags != os.O_WRONLY|os.O_CREATE|os.O_EXCL {
		t.Fatalf("open flags = %#x", openFlags)
	}
	if createMode != 0o600 {
		t.Fatalf("creation mode = %04o", createMode)
	}
	wantEvents := []string{"open", "write", "data sync", "chmod", "metadata sync", "close"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	requireFileMode(t, path, 0o555)
}

func TestWriteExclusivePreservesExistingDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	sentinel := []byte("do not replace")
	if err := os.WriteFile(path, sentinel, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(path, []byte("replacement"), 0o555); err == nil {
		t.Fatal("writeExclusive unexpectedly replaced an existing destination")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("existing content = %q", got)
	}
	requireFileMode(t, path, 0o640)
}

func TestWriteExclusiveRemovesDestinationAfterOperationFailure(t *testing.T) {
	tests := []struct {
		name       string
		failAt     string
		wantEvents []string
	}{
		{name: "write", failAt: "write", wantEvents: []string{"write", "close"}},
		{name: "data sync", failAt: "data sync", wantEvents: []string{"write", "data sync", "close"}},
		{name: "chmod", failAt: "chmod", wantEvents: []string{"write", "data sync", "chmod", "close"}},
		{name: "metadata sync", failAt: "metadata sync", wantEvents: []string{"write", "data sync", "chmod", "metadata sync", "close"}},
		{name: "close", failAt: "close", wantEvents: []string{"write", "data sync", "chmod", "metadata sync", "close"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "staged")
			cause := errors.New(test.failAt)
			var events []string
			operations := failingExclusiveWriteOperations(test.failAt, cause, &events, os.Remove)
			err := writeExclusiveWithOperations(path, []byte("content"), 0o555, operations)
			if !errors.Is(err, cause) {
				t.Fatalf("error = %v, want cause %v", err, cause)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("created destination remains after failure: %v", statErr)
			}
			if strings.Join(events, ",") != strings.Join(test.wantEvents, ",") {
				t.Fatalf("events = %v, want %v", events, test.wantEvents)
			}
		})
	}
}

func TestWriteExclusiveJoinsCleanupFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staged")
	primary := errors.New("write failure")
	closeFailure := errors.New("cleanup close failure")
	removeFailure := errors.New("cleanup remove failure")
	var events []string
	operations := failingExclusiveWriteOperations("write", primary, &events, func(string) error {
		return removeFailure
	})
	originalOpen := operations.open
	operations.open = func(path string, flags int, mode os.FileMode) (exclusiveWriteFile, error) {
		file, err := originalOpen(path, flags, mode)
		if err != nil {
			return nil, err
		}
		wrapped := file.(*testExclusiveWriteFile)
		originalClose := wrapped.close
		wrapped.close = func() error {
			_ = originalClose()
			return closeFailure
		}
		return wrapped, nil
	}
	err := writeExclusiveWithOperations(path, []byte("content"), 0o555, operations)
	for _, cause := range []error{primary, closeFailure, removeFailure} {
		if !errors.Is(err, cause) {
			t.Fatalf("error = %v, missing cause %v", err, cause)
		}
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("remove failure did not retain the fixture: %v", statErr)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

type testExclusiveWriteFile struct {
	write func([]byte) (int, error)
	sync  func() error
	chmod func(os.FileMode) error
	close func() error
}

func (file *testExclusiveWriteFile) Write(content []byte) (int, error) { return file.write(content) }
func (file *testExclusiveWriteFile) Sync() error                       { return file.sync() }
func (file *testExclusiveWriteFile) Chmod(mode os.FileMode) error      { return file.chmod(mode) }
func (file *testExclusiveWriteFile) Close() error                      { return file.close() }

func failingExclusiveWriteOperations(failAt string, cause error, events *[]string, remove func(string) error) exclusiveWriteOperations {
	return exclusiveWriteOperations{
		open: func(path string, flags int, mode os.FileMode) (exclusiveWriteFile, error) {
			file, err := os.OpenFile(path, flags, mode)
			if err != nil {
				return nil, err
			}
			syncCalls := 0
			return &testExclusiveWriteFile{
				write: func(content []byte) (int, error) {
					*events = append(*events, "write")
					if failAt == "write" {
						return 0, cause
					}
					return file.Write(content)
				},
				sync: func() error {
					syncCalls++
					operation := "data sync"
					if syncCalls == 2 {
						operation = "metadata sync"
					}
					*events = append(*events, operation)
					if failAt == operation {
						return cause
					}
					return file.Sync()
				},
				chmod: func(mode os.FileMode) error {
					*events = append(*events, "chmod")
					if failAt == "chmod" {
						return cause
					}
					return file.Chmod(mode)
				},
				close: func() error {
					*events = append(*events, "close")
					closeErr := file.Close()
					if failAt == "close" {
						return cause
					}
					return closeErr
				},
			}, nil
		},
		remove: remove,
	}
}

func requireFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %04o, want %04o", filepath.Base(path), info.Mode().Perm(), want)
	}
}
