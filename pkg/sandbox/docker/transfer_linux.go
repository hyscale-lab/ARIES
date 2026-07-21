//go:build linux

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type transferTestHooks struct {
	afterUploadOpen    func()
	beforeDownloadWalk func()
}

func (s *Sandbox) uploadFile(ctx context.Context, source, destination string) error {
	containerDestination, err := cleanContainerPath(destination)
	if err != nil {
		return fmt.Errorf("validate Docker upload destination: %w", err)
	}
	sourceFile, before, err := openRegularNoFollow(source)
	if err != nil {
		return fmt.Errorf("open Docker upload source: %w", err)
	}
	defer sourceFile.Close()
	if s.testHooks.afterUploadOpen != nil {
		s.testHooks.afterUploadOpen()
	}

	staged, err := os.CreateTemp(s.artifactDir, "upload-*")
	if err != nil {
		return fmt.Errorf("create private Docker upload stage: %w", err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	defer staged.Close()
	if err := staged.Chmod(before.Mode().Perm()); err != nil {
		return fmt.Errorf("set Docker upload stage mode: %w", err)
	}
	copied, err := io.Copy(staged, sourceFile)
	if err != nil {
		return fmt.Errorf("stage Docker upload bytes: %w", err)
	}
	after, err := sourceFile.Stat()
	if err != nil {
		return fmt.Errorf("reinspect Docker upload source: %w", err)
	}
	if !stableFile(before, after, copied) {
		return errors.New("Docker upload source changed while being staged")
	}
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("sync Docker upload stage: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("close Docker upload stage: %w", err)
	}
	if _, err := runChecked(ctx, s.cli, nil, "container", "cp", stagedPath, s.containerID+":"+containerDestination); err != nil {
		return fmt.Errorf("upload staged file to Docker task container: %w", err)
	}
	return nil
}

func (s *Sandbox) downloadFile(ctx context.Context, source, destination string) error {
	containerSource, err := cleanContainerPath(source)
	if err != nil {
		return fmt.Errorf("validate Docker download source: %w", err)
	}
	stageDir, err := os.MkdirTemp(s.artifactDir, "download-*")
	if err != nil {
		return fmt.Errorf("create private Docker download stage: %w", err)
	}
	defer os.RemoveAll(stageDir)
	stagePath := filepath.Join(stageDir, "payload")
	if _, err := runChecked(ctx, s.cli, nil, "container", "cp", s.containerID+":"+containerSource, stagePath); err != nil {
		return fmt.Errorf("download file from Docker task container: %w", err)
	}
	stageFile, before, err := openRegularNoFollow(stagePath)
	if err != nil {
		return fmt.Errorf("open staged Docker download: %w", err)
	}
	defer stageFile.Close()
	if err := publishBeneath(s.outputDir, destination, stageFile, before, s.testHooks.beforeDownloadWalk); err != nil {
		return fmt.Errorf("publish Docker download: %w", err)
	}
	return nil
}

func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return nil, nil, errors.New("path is empty or contains NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	fd, err := syscall.Open(absolute, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), absolute)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, errors.New("path is not a regular file")
	}
	return file, info, nil
}

func stableFile(before, after os.FileInfo, copied int64) bool {
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	return beforeOK && afterOK &&
		beforeStat.Ctim == afterStat.Ctim &&
		os.SameFile(before, after) &&
		before.Size() == copied && after.Size() == copied &&
		before.Mode() == after.Mode() && before.ModTime() == after.ModTime()
}

func publishBeneath(root, destination string, source *os.File, before os.FileInfo, hook func()) (returnErr error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("host destination is outside the configured output directory")
	}
	components := strings.Split(relative, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.ContainsRune(component, 0) {
			return errors.New("host destination contains an unsafe path component")
		}
	}

	rootFD, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open output root: %w", err)
	}
	currentFD := rootFD
	defer func() {
		if currentFD != rootFD {
			_ = syscall.Close(currentFD)
		}
		_ = syscall.Close(rootFD)
	}()
	if hook != nil {
		hook()
	}
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := syscall.Openat(currentFD, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if errors.Is(openErr, syscall.ENOENT) {
			if mkdirErr := syscall.Mkdirat(currentFD, component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, syscall.EEXIST) {
				return fmt.Errorf("create output directory %q: %w", component, mkdirErr)
			}
			nextFD, openErr = syscall.Openat(currentFD, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			return fmt.Errorf("open output directory %q without following symlinks: %w", component, openErr)
		}
		if currentFD != rootFD {
			_ = syscall.Close(currentFD)
		}
		currentFD = nextFD
	}

	name := components[len(components)-1]
	fd, err := syscall.Openat(currentFD, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, uint32(before.Mode().Perm()))
	if err != nil {
		return fmt.Errorf("create output file exclusively: %w", err)
	}
	published := os.NewFile(uintptr(fd), name)
	removePublished := true
	defer func() {
		if closeErr := published.Close(); returnErr == nil && closeErr != nil {
			returnErr = closeErr
		}
		if removePublished {
			_ = syscall.Unlinkat(currentFD, name)
		}
	}()
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	copied, err := io.Copy(published, source)
	if err != nil {
		return err
	}
	after, err := source.Stat()
	if err != nil {
		return err
	}
	if !stableFile(before, after, copied) {
		return errors.New("staged Docker download changed while being published")
	}
	if err := published.Sync(); err != nil {
		return err
	}
	removePublished = false
	return nil
}
