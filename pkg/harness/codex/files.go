package codex

import (
	"archive/tar"
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const maxBinaryBytes = 512 << 20

type stagedFile struct {
	content []byte
	mode    int64
}

func (manager *Manager) runtimeArchive(active *session, configuration, environments []byte) ([]byte, error) {
	identity, err := readSource(active.endpoint.IdentitySourceFile, 0o600, maxOutputBytes)
	if err != nil {
		return nil, fmt.Errorf("read Codex SSH identity: %w", err)
	}
	defer clear(identity)
	hosts, err := readSource(active.endpoint.KnownHostsSourceFile, 0o600, maxOutputBytes)
	if err != nil {
		return nil, fmt.Errorf("read Codex SSH known hosts: %w", err)
	}
	helper, err := readStaticExecutable(active.endpoint.ClientSourceFile)
	if err != nil {
		return nil, fmt.Errorf("read Codex SSH helper: %w", err)
	}
	binary, err := readStaticExecutable(manager.codexSource)
	if err != nil {
		return nil, fmt.Errorf("read pinned Codex: %w", err)
	}
	files := map[string]stagedFile{
		configPath: {configuration, 0o600}, environmentsPath: {environments, 0o600},
		modelKeyPath: {active.apiKey, 0o600}, identityPath: {identity, 0o600}, knownHostsPath: {hosts, 0o600},
		agentWrapperPath: {agentWrapperScript(active.model.APIKeyEnv), 0o500},
		codexPath:        {binary, 0o500}, clientPath: {helper, 0o500},
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	directories := map[string]int64{"run/aries": 0o700, "run/aries/codex": 0o700, "run/aries/codex/home": 0o700, "run/aries/ssh": 0o700}
	for directory := strings.TrimPrefix(active.endpoint.Workdir, "/"); directory != "" && directory != "."; directory = path.Dir(directory) {
		directories[directory] = 0o755
	}
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: directories[name]}); err != nil {
			clear(output.Bytes())
			return nil, err
		}
	}
	names = names[:0]
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		file := files[name]
		if err := writer.WriteHeader(&tar.Header{Name: strings.TrimPrefix(name, "/"), Typeflag: tar.TypeReg, Mode: file.mode, Size: int64(len(file.content))}); err != nil {
			clear(output.Bytes())
			return nil, err
		}
		if _, err := writer.Write(file.content); err != nil {
			clear(output.Bytes())
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		clear(output.Bytes())
		return nil, err
	}
	return output.Bytes(), nil
}

// No interpreter or dynamic dependency may be required in either the generic
// harness image or an arbitrary task image. The exact CLI version is then
// checked inside the isolated container before Start succeeds.
func readStaticExecutable(filename string) ([]byte, error) {
	content, err := readSource(filename, 0, maxBinaryBytes)
	if err != nil {
		return nil, err
	}
	binary, err := elf.NewFile(bytes.NewReader(content))
	if err != nil {
		return nil, errors.New("executable must be a static Linux ELF file")
	}
	defer binary.Close()
	if binary.Type != elf.ET_EXEC && binary.Type != elf.ET_DYN || binary.Machine != elf.EM_X86_64 && binary.Machine != elf.EM_AARCH64 {
		return nil, errors.New("unsupported Linux executable type or architecture")
	}
	for _, program := range binary.Progs {
		if program.Type == elf.PT_INTERP {
			return nil, errors.New("executable requires a dynamic interpreter")
		}
	}
	libraries, err := binary.ImportedLibraries()
	if err != nil || len(libraries) != 0 {
		return nil, errors.New("executable has dynamic dependencies")
	}
	return content, nil
}

// Open the final component without following symlinks and check the same
// descriptor before and after the bounded read. Mode zero means executable.
func readSource(filename string, mode os.FileMode, limit int64) ([]byte, error) {
	fd, err := syscall.Open(filename, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filename)
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit || mode != 0 && before.Mode().Perm() != mode || mode == 0 && before.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("source must be one bounded regular file with the required mode")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) > limit {
		clear(content)
		return nil, errors.New("source read exceeded its bound")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		clear(content)
		return nil, errors.New("source changed while being read")
	}
	return content, nil
}

func ensurePrivateDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return errors.New("private directory contains a symbolic link")
	}
	return os.Chmod(directory, 0o700)
}

func writeArtifact(filename string, content []byte) error {
	if err := ensurePrivateDirectory(filepath.Dir(filename)); err != nil {
		return err
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = os.Remove(filename)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(filename)
		return err
	}
	return file.Close()
}
