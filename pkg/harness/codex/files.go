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
	"slices"
	"strings"
	"syscall"

	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const maxBinaryBytes = 512 << 20
const maxCABundleBytes = 4 << 20
const maxRolloutBytes = 256 << 20

type namedFile struct {
	name string
	data []byte
}

func (manager *Manager) runtimeArchive(active *harness.Occurrence, configuration, environments []byte) ([]byte, error) {
	modelURL, err := url.Parse(active.Model.BaseURL)
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
	// The session stages its helper copy read-only, as for OpenClaw.
	helper, err := privatefiles.Read(active.Endpoint.ClientSourceFile, 0o555)
	if err != nil {
		return nil, fmt.Errorf("read Codex SSH helper: %w", err)
	}
	binary, err := readStaticExecutable(manager.codexSource)
	if err != nil {
		return nil, fmt.Errorf("read pinned Codex: %w", err)
	}
	files := map[string]harness.ArchiveFile{
		configPath: {Content: configuration, Mode: 0o600}, environmentsPath: {Content: environments, Mode: 0o600},
		modelKeyPath:     {Content: active.Credentials.Get("model"), Mode: 0o600},
		agentWrapperPath: {Content: agentWrapperScript(active.Model.APIKeyEnv), Mode: 0o500},
		codexPath:        {Content: binary, Mode: 0o500}, clientPath: {Content: helper, Mode: 0o500},
	}
	directories := map[string]int64{"run/aries": 0o700, "run/aries/codex": 0o700, "run/aries/codex/home": 0o700}
	if len(caBundle) != 0 {
		files["/etc/ssl/certs/ca-certificates.crt"] = harness.ArchiveFile{Content: caBundle, Mode: 0o644}
		for _, directory := range []string{"etc", "etc/ssl", "etc/ssl/certs"} {
			directories[directory] = 0o755
		}
	}
	// Codex runs with --cd on the task workdir, which must exist locally even
	// though every tool call executes in the sandbox.
	for directory := strings.TrimPrefix(active.Endpoint.Workdir, "/"); directory != "" && directory != "."; directory = path.Dir(directory) {
		directories[directory] = 0o755
	}
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	slices.Sort(names) // Parents sort before their children.
	ordered := make([]harness.ArchiveDirectory, 0, len(names))
	for _, name := range names {
		ordered = append(ordered, harness.ArchiveDirectory{Name: name, Mode: directories[name]})
	}
	staged := make(map[string]harness.ArchiveFile, len(files))
	for name, file := range files {
		staged[strings.TrimPrefix(name, "/")] = file
	}
	return harness.StageArchive(ordered, staged)
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

// copyRollouts reads every native rollout Codex wrote under its private home.
// The runtime is ARIES-owned and still running, so the deployment archive
// operation reads it without executing anything inside. A missing sessions directory
// yields no rollouts.
func (manager *Manager) copyRollouts(ctx context.Context, id string) ([]namedFile, error) {
	content, _, err := manager.runtime.Options.Deployment.DownloadArchive(ctx, id, codexHome+"/sessions")
	if errors.Is(err, runner.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer content.Close()
	var rollouts []namedFile
	total := int64(0)
	reader := tar.NewReader(content)
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
