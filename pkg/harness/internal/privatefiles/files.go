// Package privatefiles provides shared credential and artifact file handling for harnesses.
package privatefiles

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maxPrivateFileBytes = 16 << 20

// Read rejects symlinks and files that change, exceed the bound, or have unsafe modes.
func Read(path string, mode os.FileMode) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != mode || before.Size() < 1 || before.Size() > maxPrivateFileBytes {
		return nil, errors.New("private source is not one bounded regular file with the required mode")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxPrivateFileBytes+1))
	if err != nil || len(content) > maxPrivateFileBytes {
		return nil, errors.New("read private source within bound")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return nil, errors.New("private source changed while being read")
	}
	return content, nil
}

// WriteArtifact creates private immutable evidence; identical retries are allowed.
func WriteArtifact(path string, content []byte) error {
	if err := EnsureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := Read(path, 0o600)
		if readErr == nil && bytes.Equal(existing, content) {
			clear(existing)
			return nil
		}
		clear(existing)
		return fmt.Errorf("artifact %q already exists with different content", path)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

// EnsureDirectory rejects symlinked paths and makes the directory private.
func EnsureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return errors.New("private directory path contains a symbolic link")
	}
	return os.Chmod(path, 0o700)
}
