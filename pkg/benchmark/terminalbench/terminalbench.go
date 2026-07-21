package terminalbench

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	// Revision is the only Terminal-Bench 2 revision supported by the MVP.
	Revision = "2fd12b88aafdd04a52c298e3940bcb189f9766d6"

	DefaultRoot = ".cache/terminal-bench-2"

	repositoryURL   = "https://github.com/harbor-framework/terminal-bench-2.git"
	fixGitID        = "fix-git"
	fixGitTaskName  = "terminal-bench/fix-git"
	fixGitImage     = "alexgshaw/fix-git:20251031"
	fixGitImagePin  = "alexgshaw/fix-git:20251031@sha256:61e431c00c58df652287aadce5457634d9f9330cfdd153ebdf2802df0d540119"
	fixGitWorkdir   = "/app/personal-site"
	testsPath       = "/tests"
	verifierLogPath = "/logs/verifier"
)

// Options are the explicit inputs to the pinned Terminal-Bench 2 adapter.
type Options struct {
	Root      string
	TaskIDs   []string
	OutputDir string
}

// Benchmark discovers the pinned fix-git task and owns its private verifier.
type Benchmark struct {
	root           string
	taskIDs        []string
	outputDir      string
	verifyRevision bool

	mu      sync.RWMutex
	details map[string]taskDetails
}

type taskDetails struct {
	verifierFiles []verifierFile
	timeout       time.Duration
	verifierEnv   map[string]string
	workdir       string
}

type verifierFile struct {
	name        string
	source      string
	destination string
	size        int64
	mode        os.FileMode
	modTime     time.Time
	device      uint64
	inode       uint64
	digest      [sha256.Size]byte
}

type taskFile struct {
	SchemaVersion string          `toml:"schema_version"`
	Artifacts     []string        `toml:"artifacts"`
	Task          taskSection     `toml:"task"`
	Metadata      metadataSection `toml:"metadata"`
	Verifier      verifierSection `toml:"verifier"`
	Agent         agentSection    `toml:"agent"`
	Environment   environmentFile `toml:"environment"`
	Solution      solutionSection `toml:"solution"`
}

type taskSection struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Keywords    []string `toml:"keywords"`
	Authors     []author `toml:"authors"`
}

type author struct {
	Name  string `toml:"name"`
	Email string `toml:"email"`
}

type metadataSection struct {
	AuthorName            string   `toml:"author_name"`
	AuthorEmail           string   `toml:"author_email"`
	Difficulty            string   `toml:"difficulty"`
	Category              string   `toml:"category"`
	Tags                  []string `toml:"tags"`
	ExpertTimeEstimateMin float64  `toml:"expert_time_estimate_min"`
	JuniorTimeEstimateMin float64  `toml:"junior_time_estimate_min"`
}

type verifierSection struct {
	TimeoutSeconds float64           `toml:"timeout_sec"`
	Env            map[string]string `toml:"env"`
}

type agentSection struct {
	TimeoutSeconds float64 `toml:"timeout_sec"`
}

type environmentFile struct {
	BuildTimeoutSeconds float64           `toml:"build_timeout_sec"`
	DockerImage         string            `toml:"docker_image"`
	CPUs                float64           `toml:"cpus"`
	MemoryMB            int               `toml:"memory_mb"`
	StorageMB           int               `toml:"storage_mb"`
	GPUs                int               `toml:"gpus"`
	AllowInternet       bool              `toml:"allow_internet"`
	MCPServers          []string          `toml:"mcp_servers"`
	Env                 map[string]string `toml:"env"`
}

type solutionSection struct {
	Env map[string]string `toml:"env"`
}

var _ runner.Benchmark = (*Benchmark)(nil)

// New constructs the narrow MVP adapter. Tasks verifies the dataset revision
// before reading task data.
func New(options Options) (*Benchmark, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("terminalbench root is required")
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("terminalbench task IDs are required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("terminalbench output directory is required")
	}

	seen := make(map[string]struct{}, len(options.TaskIDs))
	for _, id := range options.TaskIDs {
		if id != filepath.Base(id) || id == "." || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("invalid terminalbench task ID %q", id)
		}
		if id != fixGitID {
			return nil, fmt.Errorf("terminalbench task %q is unsupported by the MVP; only %q is pinned", id, fixGitID)
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate terminalbench task ID %q", id)
		}
		seen[id] = struct{}{}
	}

	return &Benchmark{
		root:           filepath.Clean(options.Root),
		taskIDs:        slices.Clone(options.TaskIDs),
		outputDir:      filepath.Clean(options.OutputDir),
		verifyRevision: true,
		details:        make(map[string]taskDetails, len(options.TaskIDs)),
	}, nil
}

// Tasks loads the selected task directories after verifying the pinned checkout.
func (b *Benchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	if err := VerifyRevision(ctx, b.root); err != nil {
		return nil, err
	}

	tasks := make([]core.Task, 0, len(b.taskIDs))
	details := make(map[string]taskDetails, len(b.taskIDs))
	for _, id := range b.taskIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, private, err := loadTask(b.root, id)
		if err != nil {
			return nil, fmt.Errorf("load terminalbench task %q: %w", id, err)
		}
		tasks = append(tasks, task)
		details[id] = private
	}

	b.mu.Lock()
	b.details = details
	b.mu.Unlock()
	return tasks, nil
}

func loadTask(root, id string) (core.Task, taskDetails, error) {
	taskDir := filepath.Join(root, id)
	info, err := os.Stat(taskDir)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("open task directory: %w", err)
	}
	if !info.IsDir() {
		return core.Task{}, taskDetails{}, errors.New("task path is not a directory")
	}

	var parsed taskFile
	meta, err := toml.DecodeFile(filepath.Join(taskDir, "task.toml"), &parsed)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("parse task.toml: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) != 0 {
		return core.Task{}, taskDetails{}, fmt.Errorf("task.toml contains unsupported field %q", undecoded[0].String())
	}
	if err := validateTaskFile(id, parsed); err != nil {
		return core.Task{}, taskDetails{}, err
	}

	instructionBytes, err := os.ReadFile(filepath.Join(taskDir, "instruction.md"))
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("read instruction.md: %w", err)
	}
	instruction := strings.TrimSpace(string(instructionBytes))
	if instruction == "" {
		return core.Task{}, taskDetails{}, errors.New("instruction.md is empty")
	}

	workdir, err := finalWorkdir(filepath.Join(taskDir, "environment", "Dockerfile"))
	if err != nil {
		return core.Task{}, taskDetails{}, err
	}
	if workdir != fixGitWorkdir {
		return core.Task{}, taskDetails{}, fmt.Errorf("unsupported fix-git workdir %q; want %q", workdir, fixGitWorkdir)
	}

	testsDir := filepath.Join(taskDir, "tests")
	verifierFiles := make([]verifierFile, 0, 2)
	for _, name := range []string{"test.sh", "test_outputs.py"} {
		file, err := captureVerifierFile(filepath.Join(testsDir, name), filepath.Join(testsPath, name))
		if err != nil {
			return core.Task{}, taskDetails{}, fmt.Errorf("capture verifier file %q: %w", name, err)
		}
		verifierFiles = append(verifierFiles, file)
	}

	env := cloneMap(parsed.Environment.Env)
	return core.Task{
			ID:          id,
			Instruction: instruction,
			Environment: core.Environment{
				Image:        fixGitImagePin,
				Workdir:      workdir,
				CPU:          parsed.Environment.CPUs,
				MemoryMB:     parsed.Environment.MemoryMB,
				StorageMB:    parsed.Environment.StorageMB,
				GPUs:         parsed.Environment.GPUs,
				AllowNetwork: parsed.Environment.AllowInternet,
				Env:          env,
			},
		}, taskDetails{
			verifierFiles: verifierFiles,
			timeout:       durationSeconds(parsed.Verifier.TimeoutSeconds),
			verifierEnv:   cloneMap(parsed.Verifier.Env),
			workdir:       workdir,
		}, nil
}

func captureVerifierFile(source, destination string) (verifierFile, error) {
	info, file, err := openRegularFile(source)
	if err != nil {
		return verifierFile{}, err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return verifierFile{}, fmt.Errorf("hash verifier file: %w", err)
	}
	device, inode, err := fileIdentity(info)
	if err != nil {
		return verifierFile{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return verifierFile{
		name:        filepath.Base(source),
		source:      source,
		destination: destination,
		size:        info.Size(),
		mode:        info.Mode(),
		modTime:     info.ModTime(),
		device:      device,
		inode:       inode,
		digest:      digest,
	}, nil
}

func validateVerifierFile(expected verifierFile) error {
	info, file, err := openRegularFile(expected.source)
	if err != nil {
		return err
	}
	defer file.Close()
	device, inode, err := fileIdentity(info)
	if err != nil {
		return err
	}
	if info.Size() != expected.size || info.Mode() != expected.mode || !info.ModTime().Equal(expected.modTime) || device != expected.device || inode != expected.inode {
		return errors.New("verifier file metadata changed after task discovery")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash verifier file: %w", err)
	}
	if !slices.Equal(hash.Sum(nil), expected.digest[:]) {
		return errors.New("verifier file content changed after task discovery")
	}
	return nil
}

func openRegularFile(path string) (os.FileInfo, *os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect verifier file: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("verifier file is a symlink")
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errors.New("verifier file is not regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open verifier file: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("stat open verifier file: %w", err)
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() {
		file.Close()
		return nil, nil, errors.New("verifier file changed while opening")
	}
	current, err := os.Lstat(path)
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("reinspect verifier file: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(after, current) {
		file.Close()
		return nil, nil, errors.New("verifier file changed while opening")
	}
	return after, file, nil
}

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("verifier file identity is unavailable")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func validateTaskFile(id string, parsed taskFile) error {
	if parsed.SchemaVersion != "1.1" {
		return fmt.Errorf("unsupported task schema_version %q; want %q", parsed.SchemaVersion, "1.1")
	}
	if id != fixGitID || parsed.Task.Name != fixGitTaskName {
		return fmt.Errorf("unsupported task identity directory=%q name=%q", id, parsed.Task.Name)
	}
	if len(parsed.Artifacts) != 0 {
		return errors.New("task artifacts are unsupported by the MVP")
	}
	if parsed.Environment.DockerImage != fixGitImage {
		return fmt.Errorf("unsupported fix-git docker image %q; want %q", parsed.Environment.DockerImage, fixGitImage)
	}
	if parsed.Environment.CPUs != 1 || parsed.Environment.MemoryMB != 2048 || parsed.Environment.StorageMB != 10240 || parsed.Environment.GPUs != 0 {
		return fmt.Errorf("unsupported fix-git resources cpu=%v memory_mb=%d storage_mb=%d gpus=%d", parsed.Environment.CPUs, parsed.Environment.MemoryMB, parsed.Environment.StorageMB, parsed.Environment.GPUs)
	}
	if parsed.Environment.BuildTimeoutSeconds != 600 || parsed.Agent.TimeoutSeconds != 900 || parsed.Verifier.TimeoutSeconds != 900 {
		return fmt.Errorf("unsupported fix-git timeouts build=%v agent=%v verifier=%v", parsed.Environment.BuildTimeoutSeconds, parsed.Agent.TimeoutSeconds, parsed.Verifier.TimeoutSeconds)
	}
	if !parsed.Environment.AllowInternet {
		return errors.New("fix-git requires environment.allow_internet = true")
	}
	if len(parsed.Environment.MCPServers) != 0 {
		return errors.New("environment.mcp_servers is unsupported by the MVP")
	}
	if len(parsed.Solution.Env) != 0 {
		return errors.New("solution.env is unsupported by the MVP")
	}
	return nil
}

func finalWorkdir(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open environment/Dockerfile: %w", err)
	}
	defer f.Close()

	var workdir string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.EqualFold(fields[0], "WORKDIR") {
			if len(fields) != 2 || !filepath.IsAbs(fields[1]) || strings.ContainsAny(fields[1], "$\\") {
				return "", fmt.Errorf("unsupported WORKDIR directive %q", line)
			}
			workdir = filepath.Clean(fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read environment/Dockerfile: %w", err)
	}
	if workdir == "" {
		return "", errors.New("environment/Dockerfile has no WORKDIR")
	}
	return workdir, nil
}

func cloneMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func durationSeconds(seconds float64) time.Duration {
	return time.Duration(seconds) * time.Second
}

// VerifyRevision confirms that root is the exact detached pinned checkout.
func VerifyRevision(ctx context.Context, root string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("verify terminalbench checkout %q: %w", root, err)
	}
	got := strings.TrimSpace(string(output))
	if got != Revision {
		return fmt.Errorf("terminalbench checkout %q is revision %q; want pinned %q", root, got, Revision)
	}
	status := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=all")
	output, err = status.Output()
	if err != nil {
		return fmt.Errorf("inspect terminalbench checkout %q: %w", root, err)
	}
	if len(output) != 0 {
		return fmt.Errorf("terminalbench checkout %q has local changes; the pinned dataset must be clean", root)
	}
	return nil
}
