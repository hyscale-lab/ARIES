package app

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

func loadLocalAPIKeySource(path string) (*apiKeySource, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s: %w", localAPIKeyFile, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%s must not be a symbolic link", localAPIKeyFile)
	}
	if err := validateKeyFile(info); err != nil {
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

	info, err = file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect opened %s: %w", localAPIKeyFile, err)
	}
	if err := validateKeyFile(info); err != nil {
		return nil, false, err
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

	if int64(len(content)) != info.Size() {
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

func validateKeyFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", localAPIKeyFile)
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return fmt.Errorf("%s permissions are %04o; require owner read access and no group or world permissions", localAPIKeyFile, info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has unsupported file metadata", localAPIKeyFile)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s must be owned by the current user", localAPIKeyFile)
	}
	if info.Size() < 0 || info.Size() > maxAPIKeyBytes+1 {
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
