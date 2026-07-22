package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

const (
	localAPIKeyFile = "DEEPSEEK_API.key"
	deepSeekAPIKey  = "DEEPSEEK_API_KEY"
	maxAPIKeyBytes  = 16 << 10
)

type apiKeySource struct {
	mu    sync.Mutex
	value []byte
}

type keyFileIdentity struct {
	device   uint64
	inode    uint64
	uid      uint32
	size     int64
	mode     os.FileMode
	modified int64
}

func loadLocalAPIKeySource(path string) (*apiKeySource, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s: %w", localAPIKeyFile, err)
	}
	beforeIdentity, err := keyIdentity(before)
	if err != nil {
		return nil, false, err
	}
	if err := validateKeyFile(beforeIdentity); err != nil {
		return nil, false, err
	}

	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open %s without following links: %w", localAPIKeyFile, err)
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = syscall.Close(descriptor)
		return nil, false, fmt.Errorf("open %s: invalid file descriptor", localAPIKeyFile)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect opened %s: %w", localAPIKeyFile, err)
	}
	openedIdentity, err := keyIdentity(opened)
	if err != nil {
		return nil, false, err
	}
	if err := validateKeyFile(openedIdentity); err != nil {
		return nil, false, err
	}
	if beforeIdentity != openedIdentity {
		return nil, false, fmt.Errorf("%s changed while it was opened", localAPIKeyFile)
	}

	content, err := io.ReadAll(io.LimitReader(file, maxAPIKeyBytes+2))
	if err != nil {
		clear(content)
		return nil, false, fmt.Errorf("read %s: %w", localAPIKeyFile, err)
	}
	owned := false
	defer func() {
		if !owned {
			clear(content)
		}
	}()
	if len(content) > maxAPIKeyBytes+1 {
		return nil, false, fmt.Errorf("%s exceeds the 16 KiB key bound", localAPIKeyFile)
	}

	openedAfter, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("reinspect opened %s: %w", localAPIKeyFile, err)
	}
	pathAfter, err := os.Lstat(path)
	if err != nil {
		return nil, false, fmt.Errorf("reinspect %s: %w", localAPIKeyFile, err)
	}
	openedAfterIdentity, err := keyIdentity(openedAfter)
	if err != nil {
		return nil, false, err
	}
	pathAfterIdentity, err := keyIdentity(pathAfter)
	if err != nil {
		return nil, false, err
	}
	if beforeIdentity != openedAfterIdentity || beforeIdentity != pathAfterIdentity {
		return nil, false, fmt.Errorf("%s identity, size, or modification time changed while it was read", localAPIKeyFile)
	}
	if int64(len(content)) != beforeIdentity.size {
		return nil, false, fmt.Errorf("%s size changed while it was read", localAPIKeyFile)
	}

	if len(content) != 0 && content[len(content)-1] == '\n' {
		content[len(content)-1] = 0
		content = content[:len(content)-1]
	}
	if len(content) == 0 {
		return nil, false, fmt.Errorf("%s is empty", localAPIKeyFile)
	}
	if len(content) > maxAPIKeyBytes {
		return nil, false, fmt.Errorf("%s exceeds the 16 KiB key bound", localAPIKeyFile)
	}
	if bytes.ContainsAny(content, "\x00\r\n") {
		return nil, false, fmt.Errorf("%s must contain one key with at most one terminal LF", localAPIKeyFile)
	}

	owned = true
	return &apiKeySource{value: content}, true, nil
}

func keyIdentity(info os.FileInfo) (keyFileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return keyFileIdentity{}, fmt.Errorf("%s has unsupported file metadata", localAPIKeyFile)
	}
	return keyFileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), uid: stat.Uid,
		size: info.Size(), mode: info.Mode(), modified: info.ModTime().UnixNano(),
	}, nil
}

func validateKeyFile(identity keyFileIdentity) error {
	if !identity.mode.IsRegular() {
		return fmt.Errorf("%s must be a regular file", localAPIKeyFile)
	}
	if identity.mode.Perm() != 0o600 {
		return fmt.Errorf("%s permissions are %04o; want 0600", localAPIKeyFile, identity.mode.Perm())
	}
	if identity.uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s must be owned by the current user", localAPIKeyFile)
	}
	if identity.size < 0 || identity.size > maxAPIKeyBytes+1 {
		return fmt.Errorf("%s exceeds the 16 KiB key bound", localAPIKeyFile)
	}
	return nil
}

func (source *apiKeySource) Lookup(name string) ([]byte, bool) {
	if source == nil || name != deepSeekAPIKey {
		return nil, false
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.value) == 0 {
		return nil, false
	}
	return bytes.Clone(source.value), true
}

func (source *apiKeySource) Clear() {
	if source == nil {
		return
	}
	source.mu.Lock()
	if source.value != nil {
		clear(source.value[:cap(source.value)])
		source.value = nil
	}
	source.mu.Unlock()
}

func environmentAPIKeyLookup(name string) ([]byte, bool) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil, false
	}
	return []byte(value), true
}
