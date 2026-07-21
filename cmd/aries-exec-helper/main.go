package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/hyscale-lab/aries/pkg/sandbox/docker/execproto"
)

const maxIO = 16 << 20

const (
	workspaceOwnerTokenBytes = 32
	workspaceOwnerMarker     = ".aries-workspace-owner-v1"
)

type sharedLimit struct {
	mu        sync.Mutex
	remaining int
	exceeded  bool
}

type limitedBuffer struct {
	limit  *sharedLimit
	buffer bytes.Buffer
}

func (b *limitedBuffer) Write(content []byte) (int, error) {
	b.limit.mu.Lock()
	defer b.limit.mu.Unlock()
	consumed := len(content)
	if len(content) > b.limit.remaining {
		b.limit.exceeded = true
		content = content[:b.limit.remaining]
	}
	b.limit.remaining -= len(content)
	if _, err := b.buffer.Write(content); err != nil {
		return 0, err
	}
	return consumed, nil
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
}

func run() error {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--") {
		return runTrustedFileOperationWithInput(os.Args[1:], os.Stdin)
	}
	if len(os.Args) < 3 {
		return errors.New("usage: aries-exec-helper SOCKET COMMAND [ARG...]")
	}
	connection, err := net.Dial("unix", os.Args[1])
	if err != nil {
		return fmt.Errorf("connect host: %w", err)
	}
	defer connection.Close()
	if err := execproto.WriteHello(connection); err != nil {
		return err
	}
	input, err := execproto.ReadInput(connection, maxIO)
	if err != nil {
		return err
	}
	command := exec.Command(os.Args[2], os.Args[3:]...)
	command.Stdin = bytes.NewReader(input)
	limit := &sharedLimit{remaining: maxIO}
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	command.Stdout = stdout
	command.Stderr = stderr
	err = command.Run()
	limit.mu.Lock()
	exceeded := limit.exceeded
	limit.mu.Unlock()
	if exceeded {
		return errors.New("command output limit exceeded")
	}
	exitCode := 0
	if err != nil {
		exitCode = 125
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
			if exitCode < 0 {
				if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					exitCode = 128 + int(status.Signal())
				}
			}
		} else if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			exitCode = 127
		}
	}
	return execproto.WriteResult(connection, execproto.Result{ExitCode: exitCode, Stdout: stdout.buffer.Bytes(), Stderr: stderr.buffer.Bytes()})
}

func runTrustedFileOperation(args []string) error {
	return runTrustedFileOperationWithInput(args, bytes.NewReader(nil))
}

func runTrustedFileOperationWithInput(args []string, input io.Reader) error {
	switch {
	case len(args) == 2 && args[0] == "--remove-file":
		path, err := cleanAbsolutePath(args[1])
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errors.New("trusted remove target is a directory")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("trusted remove target remains")
			}
			return err
		}
		return nil
	case len(args) == 3 && args[0] == "--verify-alias":
		alias, err := cleanAbsolutePath(args[1])
		if err != nil {
			return err
		}
		target, err := cleanAbsolutePath(args[2])
		if err != nil {
			return err
		}
		aliasInfo, err := os.Lstat(alias)
		if err != nil || aliasInfo.Mode()&os.ModeSymlink == 0 {
			return errors.New("trusted alias is not a symbolic link")
		}
		linked, err := os.Readlink(alias)
		if err != nil || linked != target {
			return errors.New("trusted alias has an unexpected target")
		}
		resolvedAlias, err := os.Stat(alias)
		if err != nil {
			return err
		}
		resolvedTarget, err := os.Stat(target)
		if err != nil {
			return err
		}
		if !resolvedAlias.IsDir() || !os.SameFile(resolvedAlias, resolvedTarget) {
			return errors.New("trusted alias and target are not the same directory")
		}
		return nil
	case len(args) == 3 && args[0] == "--verify-workspace":
		workdir, err := cleanAbsolutePath(args[1])
		if err != nil {
			return err
		}
		workspace, err := cleanAbsolutePath(args[2])
		if err != nil {
			return err
		}
		return verifyWorkspaceIdentity(workdir, workspace)
	case len(args) == 4 && args[0] == "--recover-workspace":
		workdir, err := cleanAbsolutePath(args[1])
		if err != nil {
			return err
		}
		workspaceRoot, err := cleanAbsolutePath(args[2])
		if err != nil {
			return err
		}
		runtimeID := args[3]
		if runtimeID == "" || filepath.Base(runtimeID) != runtimeID || runtimeID == "." || strings.ContainsRune(runtimeID, 0) {
			return errors.New("trusted runtime ID must be one clean path component")
		}
		ownerToken, err := readWorkspaceOwnerToken(input)
		if err != nil {
			return err
		}
		return recoverWorkspace(workdir, workspaceRoot, runtimeID, ownerToken)
	default:
		return errors.New("unsupported trusted file operation")
	}
}

func recoverWorkspace(workdir, workspaceRoot, runtimeID string, ownerToken []byte) error {
	runtimeRoot := filepath.Join(workspaceRoot, runtimeID)
	workspace := filepath.Join(runtimeRoot, "workspace")
	if len(ownerToken) != workspaceOwnerTokenBytes {
		return errors.New("trusted workspace ownership token has an invalid length")
	}
	if pathWithin(workdir, workspaceRoot) || pathWithin(workspaceRoot, workdir) {
		return errors.New("trusted workdir and workspace root must be disjoint")
	}
	if err := requireRealParentChain(workdir); err != nil {
		return fmt.Errorf("validate trusted workdir ancestry: %w", err)
	}
	if err := requireRealChain(workspaceRoot, false); err != nil {
		return fmt.Errorf("validate trusted workspace ancestry: %w", err)
	}
	if err := validateWorkspaceOwnerMarker(workspaceRoot, ownerToken); err != nil {
		return err
	}
	state, err := inspectOwnedRecoveryState(workdir, workspaceRoot, runtimeID)
	if err != nil {
		return err
	}

	switch state {
	case recoveryUnchanged:
	case recoveryRenamed:
		if err := os.Rename(workspace, workdir); err != nil {
			return fmt.Errorf("restore renamed task workdir: %w", err)
		}
	case recoveryAliased:
		if err := os.Remove(workdir); err != nil {
			return fmt.Errorf("remove trusted workspace alias: %w", err)
		}
		if err := os.Rename(workspace, workdir); err != nil {
			relinkErr := os.Symlink(workspace, workdir)
			return errors.Join(fmt.Errorf("restore task workdir from workspace: %w", err), wrapRecoveryError("restore workspace alias after rename failure", relinkErr))
		}
	case recoveryReverseAliased:
		if err := os.Remove(workspace); err != nil {
			return fmt.Errorf("remove trusted reverse workspace alias: %w", err)
		}
	}

	if err := removeExactEmptyDirectory(runtimeRoot); err != nil {
		return fmt.Errorf("remove exact empty runtime root: %w", err)
	}
	markerPath := filepath.Join(workspaceRoot, workspaceOwnerMarker)
	if err := os.Remove(markerPath); err != nil {
		return fmt.Errorf("remove exact workspace ownership marker: %w", err)
	}
	if err := os.Remove(workspaceRoot); err != nil {
		restoreErr := writeWorkspaceOwnerMarker(workspaceRoot, ownerToken)
		return errors.Join(fmt.Errorf("remove exact empty workspace root: %w", err), wrapRecoveryError("restore workspace ownership marker after root removal failure", restoreErr))
	}
	info, err := os.Lstat(workdir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("trusted workdir was not restored as one real directory")
	}
	if _, err := os.Lstat(workspaceRoot); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("trusted workspace root remains after recovery")
		}
		return err
	}
	return nil
}

type recoveryState uint8

const (
	recoveryUnchanged recoveryState = iota
	recoveryRenamed
	recoveryAliased
	recoveryReverseAliased
)

func inspectOwnedRecoveryState(workdir, workspaceRoot, runtimeID string) (recoveryState, error) {
	runtimeRoot := filepath.Join(workspaceRoot, runtimeID)
	workspace := filepath.Join(runtimeRoot, "workspace")
	rootEntries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return 0, err
	}
	for _, entry := range rootEntries {
		if entry.Name() != workspaceOwnerMarker && entry.Name() != runtimeID {
			return 0, fmt.Errorf("foreign entry %q exists in workspace root", entry.Name())
		}
		if entry.Name() == runtimeID && (entry.Type()&os.ModeSymlink != 0 || !entry.IsDir()) {
			return 0, errors.New("runtime root is not one real directory")
		}
	}
	runtimeEntries, err := os.ReadDir(runtimeRoot)
	runtimeMissing := errors.Is(err, os.ErrNotExist)
	if err != nil && !runtimeMissing {
		return 0, err
	}
	if !runtimeMissing {
		for _, entry := range runtimeEntries {
			if entry.Name() != "workspace" {
				return 0, fmt.Errorf("foreign entry %q exists in runtime root", entry.Name())
			}
			if entry.Type()&os.ModeSymlink == 0 && !entry.IsDir() {
				return 0, errors.New("runtime workspace is neither a directory nor a symbolic link")
			}
		}
	}
	workInfo, workErr := os.Lstat(workdir)
	workspaceInfo, workspaceErr := os.Lstat(workspace)
	workMissing := errors.Is(workErr, os.ErrNotExist)
	workspaceMissing := errors.Is(workspaceErr, os.ErrNotExist)
	if workErr != nil && !workMissing {
		return 0, workErr
	}
	if workspaceErr != nil && !workspaceMissing {
		return 0, workspaceErr
	}
	switch {
	case !workMissing && workInfo.IsDir() && workInfo.Mode()&os.ModeSymlink == 0 && workspaceMissing:
		return recoveryUnchanged, nil
	case workMissing && !workspaceMissing && workspaceInfo.IsDir() && workspaceInfo.Mode()&os.ModeSymlink == 0:
		return recoveryRenamed, nil
	case !workMissing && workInfo.Mode()&os.ModeSymlink != 0 && !workspaceMissing && workspaceInfo.IsDir() && workspaceInfo.Mode()&os.ModeSymlink == 0:
		target, err := os.Readlink(workdir)
		if err != nil || target != workspace {
			return 0, errors.New("trusted workspace alias has an unexpected target")
		}
		resolvedWork, err := os.Stat(workdir)
		if err != nil || !resolvedWork.IsDir() || !os.SameFile(resolvedWork, workspaceInfo) {
			return 0, errors.New("trusted workspace alias does not resolve to the exact workspace")
		}
		return recoveryAliased, nil
	case !workMissing && workInfo.IsDir() && workInfo.Mode()&os.ModeSymlink == 0 && !workspaceMissing && workspaceInfo.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(workspace)
		if err != nil || target != workdir {
			return 0, errors.New("trusted reverse workspace alias has an unexpected target")
		}
		resolvedWorkspace, err := os.Stat(workspace)
		if err != nil || !resolvedWorkspace.IsDir() || !os.SameFile(resolvedWorkspace, workInfo) {
			return 0, errors.New("trusted reverse workspace alias does not resolve to the exact workdir")
		}
		return recoveryReverseAliased, nil
	default:
		return 0, errors.New("trusted workspace state is ambiguous or foreign")
	}
}

func verifyWorkspaceIdentity(workdir, workspace string) error {
	workInfo, workErr := os.Lstat(workdir)
	workspaceInfo, workspaceErr := os.Lstat(workspace)
	if workErr != nil || workspaceErr != nil {
		return errors.New("trusted workspace identity path is missing")
	}
	workLink := workInfo.Mode()&os.ModeSymlink != 0
	workspaceLink := workspaceInfo.Mode()&os.ModeSymlink != 0
	if workLink == workspaceLink {
		return errors.New("trusted workspace identity requires exactly one symbolic link")
	}
	alias, target := workdir, workspace
	if workspaceLink {
		alias, target = workspace, workdir
	}
	linked, err := os.Readlink(alias)
	if err != nil || linked != target {
		return errors.New("trusted workspace alias has an unexpected target")
	}
	resolvedAlias, err := os.Stat(alias)
	if err != nil || !resolvedAlias.IsDir() {
		return errors.New("trusted workspace alias does not resolve to a directory")
	}
	resolvedTarget, err := os.Stat(target)
	if err != nil || !resolvedTarget.IsDir() || !os.SameFile(resolvedAlias, resolvedTarget) {
		return errors.New("trusted workdir and workspace are not the same directory")
	}
	return nil
}

func readWorkspaceOwnerToken(input io.Reader) ([]byte, error) {
	token, err := io.ReadAll(io.LimitReader(input, workspaceOwnerTokenBytes+1))
	if err != nil {
		return nil, err
	}
	if len(token) != workspaceOwnerTokenBytes {
		return nil, fmt.Errorf("trusted workspace ownership token has %d bytes, want %d", len(token), workspaceOwnerTokenBytes)
	}
	return token, nil
}

func validateWorkspaceOwnerMarker(workspaceRoot string, ownerToken []byte) error {
	path := filepath.Join(workspaceRoot, workspaceOwnerMarker)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open exact workspace ownership marker: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != workspaceOwnerTokenBytes {
		return errors.New("workspace ownership marker is not one exact private regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, workspaceOwnerTokenBytes+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(content, ownerToken) {
		return errors.New("workspace ownership marker does not match this prepare attempt")
	}
	return nil
}

func writeWorkspaceOwnerMarker(workspaceRoot string, ownerToken []byte) error {
	path := filepath.Join(workspaceRoot, workspaceOwnerMarker)
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(ownerToken); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func requireRealParentChain(path string) error {
	return requireRealChain(filepath.Dir(path), false)
}

func requireRealChain(path string, missingAllowed bool) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && missingAllowed {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is not one real directory", current)
		}
	}
	return nil
}

func removeExactEmptyDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is not one real directory")
	}
	return os.Remove(path)
}

func wrapRecoveryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func cleanAbsolutePath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return "", errors.New("trusted file path must be absolute, clean, non-root, and NUL-free")
	}
	return path, nil
}
