package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const (
	ariesExecutableName = "aries"
	ariesModuleLine     = "module github.com/hyscale-lab/aries"
	maxGoModBytes       = 1 << 20
)

type repositoryFileIdentity struct {
	device   uint64
	inode    uint64
	size     int64
	mode     os.FileMode
	modified int64
}

func ariesRepositoryRoot(executablePath string) (string, error) {
	if executablePath == "" {
		var err error
		executablePath, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate ARIES executable: %w", err)
		}
	}
	if !filepath.IsAbs(executablePath) || filepath.Clean(executablePath) != executablePath {
		return "", errors.New("ARIES executable path must be absolute and clean")
	}
	resolved, err := filepath.EvalSymlinks(executablePath)
	if err != nil {
		return "", fmt.Errorf("resolve ARIES executable: %w", err)
	}
	if resolved != executablePath {
		return "", errors.New("ARIES executable path must not contain symbolic links")
	}
	executableInfo, err := os.Lstat(executablePath)
	if err != nil {
		return "", fmt.Errorf("inspect ARIES executable: %w", err)
	}
	if !executableInfo.Mode().IsRegular() || executableInfo.Mode()&os.ModeSymlink != 0 || executableInfo.Mode().Perm()&0o111 == 0 {
		return "", errors.New("ARIES executable must be an executable regular non-symbolic-link file")
	}
	if filepath.Base(executablePath) != ariesExecutableName {
		return "", fmt.Errorf("ARIES executable must be named %q", ariesExecutableName)
	}
	binDirectory := filepath.Dir(executablePath)
	if filepath.Base(binDirectory) != "bin" {
		return "", errors.New("ARIES executable must be located directly in the repository bin directory")
	}
	repositoryRoot := filepath.Dir(binDirectory)
	rootInfo, err := os.Lstat(repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("inspect ARIES repository root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("ARIES repository root must be a non-symbolic-link directory")
	}
	if err := validateAriesModule(filepath.Join(repositoryRoot, "go.mod")); err != nil {
		return "", err
	}
	return repositoryRoot, nil
}

func validateAriesModule(path string) error {
	beforeInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect ARIES go.mod: %w", err)
	}
	before, err := repositoryIdentity(beforeInfo)
	if err != nil {
		return err
	}
	if !before.mode.IsRegular() || before.mode&os.ModeSymlink != 0 || before.size < 1 || before.size > maxGoModBytes {
		return errors.New("ARIES go.mod must be a bounded regular non-symbolic-link file")
	}
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open ARIES go.mod without following links: %w", err)
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = syscall.Close(descriptor)
		return errors.New("open ARIES go.mod: invalid file descriptor")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened ARIES go.mod: %w", err)
	}
	opened, err := repositoryIdentity(openedInfo)
	if err != nil {
		return err
	}
	if before != opened {
		return errors.New("ARIES go.mod changed while it was opened")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxGoModBytes+1))
	if err != nil {
		return fmt.Errorf("read ARIES go.mod: %w", err)
	}
	defer clear(content)
	if len(content) > maxGoModBytes {
		return errors.New("ARIES go.mod exceeds its byte bound")
	}
	openedAfterInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reinspect opened ARIES go.mod: %w", err)
	}
	pathAfterInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("reinspect ARIES go.mod: %w", err)
	}
	openedAfter, err := repositoryIdentity(openedAfterInfo)
	if err != nil {
		return err
	}
	pathAfter, err := repositoryIdentity(pathAfterInfo)
	if err != nil {
		return err
	}
	if before != openedAfter || before != pathAfter || int64(len(content)) != before.size {
		return errors.New("ARIES go.mod identity, size, or modification time changed while it was read")
	}
	firstLine, _, _ := bytes.Cut(content, []byte{'\n'})
	if string(firstLine) != ariesModuleLine {
		return fmt.Errorf("ARIES go.mod must declare %q", ariesModuleLine)
	}
	return nil
}

func repositoryIdentity(info os.FileInfo) (repositoryFileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return repositoryFileIdentity{}, errors.New("ARIES repository file has unsupported metadata")
	}
	return repositoryFileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), size: info.Size(),
		mode: info.Mode(), modified: info.ModTime().UnixNano(),
	}, nil
}
