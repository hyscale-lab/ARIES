package codex

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"debug/elf"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const maxBinaryBytes = 512 << 20
const maxCABundleBytes = 4 << 20
const maxRolloutBytes = 256 << 20

type namedFile struct {
	name string
	data []byte
}

type stagedFile struct {
	content []byte
	mode    int64
}

func (manager *Manager) runtimeArchive(active *session, configuration, environments []byte) ([]byte, error) {
	modelURL, err := url.Parse(active.model.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Codex model URL: %w", err)
	}
	var caBundle []byte
	if modelURL.Scheme == "https" {
		caBundle, err = manager.readCABundle()
		if err != nil {
			return nil, fmt.Errorf("read Codex HTTPS CA bundle: %w", err)
		}
	}
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
	if len(caBundle) != 0 {
		files["/etc/ssl/certs/ca-certificates.crt"] = stagedFile{caBundle, 0o644}
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	directories := map[string]int64{"run/aries": 0o700, "run/aries/codex": 0o700, "run/aries/codex/home": 0o700, "run/aries/ssh": 0o700}
	if len(caBundle) != 0 {
		for _, directory := range []string{"etc", "etc/ssl", "etc/ssl/certs"} {
			directories[directory] = 0o755
		}
	}
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

func (manager *Manager) readCABundle() ([]byte, error) {
	if manager.caBundleSource != "" {
		return readPublicCABundle(manager.caBundleSource)
	}
	// Fixed system trust locations; never consult SSL_CERT_FILE or SSL_CERT_DIR.
	// Skip symlink aliases so distributions can use their canonical bundle path.
	for _, filename := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/tls/cacert.pem",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"/etc/ssl/cert.pem",
	} {
		content, err := readPublicCABundle(filename)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
			continue
		}
		return content, err
	}
	return nil, errors.New("no regular Linux system CA bundle found")
}

func readPublicCABundle(filename string) ([]byte, error) {
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
	if !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > maxCABundleBytes || before.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("CA bundle must be one bounded regular file without group or world write access")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxCABundleBytes+1))
	defer clear(content)
	if err != nil || int64(len(content)) != before.Size() || len(content) > maxCABundleBytes {
		return nil, errors.New("CA bundle read exceeded its bound or changed size")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
		return nil, errors.New("CA bundle changed while being read")
	}
	var bundle []byte
	for {
		block, rest := pem.Decode(content)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			clear(block.Bytes)
			return nil, errors.New("CA bundle must contain only public certificate PEM blocks")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("CA bundle contains an invalid X.509 certificate")
		}
		// Stage only public certificates, excluding surrounding comments or text.
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
		content = rest
	}
	if len(bundle) == 0 {
		return nil, errors.New("CA bundle contains no X.509 certificates")
	}
	return bundle, nil
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

// copyRollouts reads every native rollout Codex wrote under its private home.
// The container is ARIES-owned and still running, so Docker's archive API
// reads it without executing anything inside. A missing sessions directory
// yields no rollouts.
func (manager *Manager) copyRollouts(ctx context.Context, containerID string) ([]namedFile, error) {
	copied, err := manager.client.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{SourcePath: codexHome + "/sessions"})
	if errdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer copied.Content.Close()
	var rollouts []namedFile
	total := int64(0)
	reader := tar.NewReader(copied.Content)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read Codex sessions archive: %w", err)
		}
		name := path.Base(header.Name)
		if header.Typeflag != tar.TypeReg || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		total += header.Size
		if header.Size < 0 || total > maxRolloutBytes {
			return nil, errors.New("Codex rollouts exceed their bound")
		}
		content, err := io.ReadAll(io.LimitReader(reader, header.Size))
		if err != nil || int64(len(content)) != header.Size {
			return nil, errors.New("Codex rollout read was truncated")
		}
		rollouts = append(rollouts, namedFile{name, content})
	}
	return rollouts, nil
}
