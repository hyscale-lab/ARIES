package bridgekit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WritePrivate creates path exclusively with mode 0600, removing it again if
// any step fails, so a credential is never left partially written.
func WritePrivate(path string, content []byte) error {
	return writeExclusive(path, content, 0o600)
}

type exclusiveWriteFile interface {
	Write([]byte) (int, error)
	Sync() error
	Chmod(os.FileMode) error
	Close() error
}

type exclusiveWriteOperations struct {
	open   func(string, int, os.FileMode) (exclusiveWriteFile, error)
	remove func(string) error
}

// StageExecutable copies the helper binary into the task artifact directory,
// verifying the source did not change underneath the read.
func StageExecutable(source, destination string) error {
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 {
		return errors.New("helper source must be a regular executable")
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	after, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != int64(len(content)) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("helper source changed while being staged")
	}
	return writeExclusive(destination, content, 0o555)
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	return writeExclusiveWithOperations(path, content, mode, exclusiveWriteOperations{
		open: func(path string, flags int, mode os.FileMode) (exclusiveWriteFile, error) {
			return os.OpenFile(path, flags, mode)
		},
		remove: os.Remove,
	})
}

func writeExclusiveWithOperations(path string, content []byte, mode os.FileMode, operations exclusiveWriteOperations) error {
	file, err := operations.open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func(primary error, needsClose bool) error {
		var closeErr error
		if needsClose {
			if err := file.Close(); err != nil {
				closeErr = fmt.Errorf("close exclusive file: %w", err)
			}
		}
		removeErr := operations.remove(path)
		if removeErr != nil {
			removeErr = fmt.Errorf("remove failed exclusive file: %w", removeErr)
		}
		return errors.Join(primary, closeErr, removeErr)
	}
	written, err := file.Write(content)
	if err == nil && written != len(content) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return cleanup(fmt.Errorf("write exclusive file: %w", err), true)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync exclusive file data: %w", err), true)
	}
	if err := file.Chmod(mode); err != nil {
		return cleanup(fmt.Errorf("chmod exclusive file: %w", err), true)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync exclusive file metadata: %w", err), true)
	}
	if err := file.Close(); err != nil {
		return cleanup(fmt.Errorf("close exclusive file: %w", err), false)
	}
	return nil
}

// EnsurePrivateDirectory creates path as a 0700 directory and refuses a path
// that runs through a symbolic link.
func EnsurePrivateDirectory(path string) error {
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
		return errors.New("directory path contains a symbolic link")
	}
	return os.Chmod(path, 0o700)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
