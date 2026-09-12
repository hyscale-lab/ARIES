package toolathlon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// fixtureTasks are the task directories the fixture checkout carries, keyed
// by name, with their task_config.json content.
var fixtureTasks = map[string]string{
	"canvas-list-test": `{"needed_mcp_servers": ["canvas", "memory"], "needed_local_tools": ["claim_done"], "max_turns": 10, "meta": {}}`,
	"excel-only":       `{"needed_mcp_servers": ["excel", "filesystem", "terminal"], "max_turns": 10}`,
	"object-form":      `{"needed_mcp_servers": {"memory": {"enabled": true}}, "max_turns": 10}`,
	"github-task":      `{"needed_mcp_servers": ["github", "filesystem"], "max_turns": 10}`,
	"k8s-task":         `{"needed_mcp_servers": ["k8s"], "max_turns": 10}`,
	"unknown-server":   `{"needed_mcp_servers": ["mystery"], "max_turns": 10}`,
	"bad-server-name":  `{"needed_mcp_servers": ["../etc"], "max_turns": 10}`,
	"no-evaluation":    `{"needed_mcp_servers": ["memory"], "max_turns": 10}`,
	"empty-task":       `{"needed_mcp_servers": ["memory"], "max_turns": 10}`,
}

// fixtureTaskEntries are the direct children of the canvas-list-test task
// directory, as find would print them: kind then name.
var fixtureTaskEntries = []dirEntry{
	{'f', "README.md"},
	{'d', "docs"},
	{'d', "evaluation"},
	{'d', "groundtruth_workspace"},
	{'d', "initial_workspace"},
	{'d', "preprocess"},
	{'f', "task_config.json"},
	{'f', "token_key_session.py"},
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFixture lays out the parts of a Toolathlon checkout the adapter
// touches and commits them, so VerifyRevision can pin against it. Files
// that would be gitignored upstream are ignored here too.
func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "configs/global_configs.py\nconfigs/token_key_session.py\n__pycache__/\nconfigs/.mcp-auth/\n")
	writeFile(t, root, "configs/global_configs_example.py", "global_configs = {'podman_or_docker': 'docker'}\n")
	writeFile(t, root, "configs/token_key_session_example.py", "canvas_domain = 'localhost:20001'\n")
	writeFile(t, root, "configs/mcp_servers/canvas.yaml", "type: stdio\n")
	writeFile(t, root, "scripts/formal_run_v0.json", `{"global_task_config": {"dump_path": "./dumps", "direct_to_dumps": true}}`)
	writeFile(t, root, "scripts/decoupled/container_preprocess.py", "print('preprocess')\n")
	writeFile(t, root, "utils/helper.py", "def helper(): pass\n")
	writeFile(t, root, "main.py", "print('main')\n")
	writeFile(t, root, "global_preparation/check_installation.py", "print('ok')\n")
	writeFile(t, root, "global_preparation/deploy_containers.sh", "echo not copied\n")
	writeFile(t, root, "local_binary/github-mcp-server", "binary-not-copied\n")
	writeFile(t, root, "deployment/k8s/kind.yaml", "not copied\n")
	for name, config := range fixtureTasks {
		base := "tasks/" + taskPool + "/" + name + "/"
		writeFile(t, root, base+"task_config.json", config)
		if name != "empty-task" {
			writeFile(t, root, base+"docs/task.md", "Find my unsubmitted assignments for "+name+".\n")
		} else {
			writeFile(t, root, base+"docs/task.md", "  \n")
		}
		if name != "no-evaluation" {
			writeFile(t, root, base+"evaluation/main.py", "import sys; sys.exit(0)\n")
		}
	}
	base := "tasks/" + taskPool + "/canvas-list-test/"
	writeFile(t, root, base+"README.md", "solution notes\n")
	writeFile(t, root, base+"groundtruth_workspace/expected.csv", "a,b\n")
	writeFile(t, root, base+"initial_workspace/todo.csv", "a,b\n")
	writeFile(t, root, base+"preprocess/main.py", "print('seed')\n")
	writeFile(t, root, base+"token_key_session.py", "canvas_domain = 'localhost:20001'\n")
	commitFixture(t, root)
	// Ignored content that must never reach the archive.
	writeFile(t, root, "configs/__pycache__/global_configs.cpython-312.pyc", "cache\n")
	writeFile(t, root, "configs/.mcp-auth/token.json", "{}\n")
	if err := materializeSiteConfigs(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func commitFixture(t *testing.T, root string) {
	t.Helper()
	commands := [][]string{
		{"init", "--quiet"},
		{"add", "."},
		{"-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture"},
	}
	for _, arguments := range commands {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
}

func fixtureGitRevision(t *testing.T, root string) string {
	t.Helper()
	output, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func baseOptions(t *testing.T, root string) Options {
	t.Helper()
	return Options{
		Root:        root,
		TaskIDs:     []string{"canvas-list-test"},
		OutputDir:   filepath.Join(t.TempDir(), "runs"),
		Revision:    fixtureGitRevision(t, root),
		Environment: core.Environment{Image: "docker.io/example/toolathlon-task-image:1016beta", CPU: 2, Env: map[string]string{"TZ": "UTC"}},
	}
}

func TestTasksLoadsTaskAndFixesTheEnvironment(t *testing.T) {
	root := writeFixture(t)
	benchmark, err := New(baseOptions(t, root))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %#v", tasks)
	}
	task := tasks[0]
	if task.ID != "canvas-list-test" {
		t.Fatalf("task ID = %q", task.ID)
	}
	if !strings.HasPrefix(task.Instruction, "Find my unsubmitted assignments for canvas-list-test.") {
		t.Fatalf("instruction = %q", task.Instruction)
	}
	if !strings.Contains(task.Instruction, agentWorkspacePath) || !strings.Contains(task.Instruction, "reply without calling any tool") {
		t.Fatalf("instruction lacks the workspace and completion notes: %q", task.Instruction)
	}
	environment := task.Environment
	if environment.Image != "docker.io/example/toolathlon-task-image:1016beta" || environment.CPU != 2 || environment.Env["TZ"] != "UTC" {
		t.Fatalf("profile environment not carried: %#v", environment)
	}
	if environment.Workdir != agentWorkspacePath || !environment.AllowNetwork {
		t.Fatalf("environment policy not fixed: %#v", environment)
	}
	if task.Timeout != 0 {
		t.Fatalf("timeout = %v, want the profile default", task.Timeout)
	}
	details := benchmark.details["canvas-list-test"]
	if !details.needsApplications || !slices.Equal(details.servers, []string{"canvas", "memory"}) {
		t.Fatalf("details = %#v", details)
	}
}

func TestTasksAcceptsObjectFormServersAndLocalOnlyTasks(t *testing.T) {
	root := writeFixture(t)
	options := baseOptions(t, root)
	options.TaskIDs = []string{"excel-only", "object-form"}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if benchmark.details["excel-only"].needsApplications {
		t.Fatal("a local-only task must not start the forwarder")
	}
	if servers := benchmark.details["object-form"].servers; !slices.Equal(servers, []string{"memory"}) {
		t.Fatalf("object-form servers = %v", servers)
	}
}

func TestTasksRejectsTasksTheSandboxCannotServe(t *testing.T) {
	root := writeFixture(t)
	cases := map[string]string{
		"github-task":     "third-party account",
		"k8s-task":        "third-party account or host runtime",
		"unknown-server":  "not in the pinned server catalogue",
		"bad-server-name": "invalid MCP server name",
		"no-evaluation":   "no evaluation directory",
		"empty-task":      "task description is empty",
	}
	for id, wantErr := range cases {
		t.Run(id, func(t *testing.T) {
			options := baseOptions(t, root)
			options.TaskIDs = []string{id}
			benchmark, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			_, err = benchmark.Tasks(context.Background())
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, wantErr)
			}
		})
	}
}

func TestTasksRejectsRevisionMismatch(t *testing.T) {
	root := writeFixture(t)
	options := baseOptions(t, root)
	options.Revision = strings.Repeat("0", 40)
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "want pinned") {
		t.Fatalf("err = %v", err)
	}
}

func TestTasksRemapsToExecutionTaskID(t *testing.T) {
	root := writeFixture(t)
	options := baseOptions(t, root)
	options.ExecutionTaskIDs = []string{"canvas-list-test-002"}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].ID != "canvas-list-test-002" {
		t.Fatalf("task ID = %q", tasks[0].ID)
	}
	if _, ok := benchmark.details["canvas-list-test-002"]; !ok {
		t.Fatal("details not keyed by the execution ID")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	root := writeFixture(t)
	cases := map[string]func(*Options){
		"missing root":       func(o *Options) { o.Root = " " },
		"missing tasks":      func(o *Options) { o.TaskIDs = nil },
		"missing output":     func(o *Options) { o.OutputDir = "" },
		"missing revision":   func(o *Options) { o.Revision = "" },
		"missing image":      func(o *Options) { o.Environment.Image = "" },
		"foreign workdir":    func(o *Options) { o.Environment.Workdir = "/root" },
		"unsafe task ID":     func(o *Options) { o.TaskIDs = []string{"../etc"} },
		"duplicate task ID":  func(o *Options) { o.TaskIDs = []string{"a", "a"} },
		"execution mismatch": func(o *Options) { o.ExecutionTaskIDs = []string{"x", "y"} },
		"execution not derived": func(o *Options) {
			o.ExecutionTaskIDs = []string{"other-001"}
		},
		"privileged port":  func(o *Options) { o.GatewayPort = 80 },
		"application port": func(o *Options) { o.GatewayPort = 20001 },
		"bad app host":     func(o *Options) { o.AppHost = "host name" },
		"bracketed host":   func(o *Options) { o.AppHost = "[::1]" },
		"negative steps":   func(o *Options) { o.MaxSteps = -1 },
		"bad model name":   func(o *Options) { o.ModelName = "deep seek" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			options := baseOptions(t, root)
			mutate(&options)
			if _, err := New(options); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	options := baseOptions(t, root)
	options.Environment.Workdir = agentWorkspacePath
	options.AppHost = "10.148.0.5"
	options.ModelName = "deepseek-flash"
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if benchmark.gatewayPort != DefaultGatewayPort || benchmark.maxSteps != DefaultMaxSteps || benchmark.appHost != "10.148.0.5" {
		t.Fatalf("defaults not applied: %#v", benchmark)
	}
	// The forwarder hands the host to asyncio.open_connection, which takes
	// IPv6 literals bare, so the validator does too.
	for _, host := range []string{"fd00::5", "2001:db8::1", "::1", "docker-host.internal"} {
		options.AppHost = host
		if _, err := New(options); err != nil {
			t.Fatalf("app host %q rejected: %v", host, err)
		}
	}
}

func TestPrepareSandboxAndEvaluateRequireLiveSandboxAndLoadedTask(t *testing.T) {
	root := writeFixture(t)
	benchmark, err := New(baseOptions(t, root))
	if err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "canvas-list-test"}
	if err := benchmark.PrepareSandbox(context.Background(), task, nil); err == nil {
		t.Fatal("expected PrepareSandbox to require a sandbox")
	}
	if _, err := benchmark.Evaluate(context.Background(), task, nil); err == nil {
		t.Fatal("expected Evaluate to require a sandbox")
	}
	sandbox := newFlowSandbox(t, "canvas-list-test", true)
	if err := benchmark.PrepareSandbox(context.Background(), task, sandbox); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("err = %v", err)
	}
	if _, err := benchmark.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err == nil || !strings.Contains(err.Error(), "not prepared") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerNamesForms(t *testing.T) {
	names, err := serverNames(json.RawMessage(`["canvas", "memory", "canvas"]`))
	if err != nil || !slices.Equal(names, []string{"canvas", "memory"}) {
		t.Fatalf("list form: %v, %v", names, err)
	}
	names, err = serverNames(json.RawMessage(`{"memory": {}, "canvas": {}}`))
	if err != nil || !slices.Equal(names, []string{"canvas", "memory"}) {
		t.Fatalf("object form: %v, %v", names, err)
	}
	names, err = serverNames(nil)
	if err != nil || names != nil {
		t.Fatalf("absent: %v, %v", names, err)
	}
	if _, err := serverNames(json.RawMessage(`"canvas"`)); err == nil {
		t.Fatal("expected a bare string to be rejected")
	}
}

func TestSetupMaterializesSiteConfigsOnAnExistingCheckout(t *testing.T) {
	root := writeFixture(t)
	for target := range siteConfigs {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(target))); err != nil {
			t.Fatal(err)
		}
	}
	if err := Setup(context.Background(), root, "https://example.invalid/toolathlon.git", fixtureGitRevision(t, root)); err != nil {
		t.Fatal(err)
	}
	for target, example := range siteConfigs {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target)))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(example)))
		if string(got) != string(want) {
			t.Fatalf("%s = %q, want the example", target, got)
		}
	}
	if err := VerifyRevision(context.Background(), root, fixtureGitRevision(t, root)); err != nil {
		t.Fatalf("site configs dirtied the checkout: %v", err)
	}
	if err := Setup(context.Background(), root, "https://example.invalid/toolathlon.git", strings.Repeat("0", 40)); err == nil {
		t.Fatal("expected a wrong pinned revision to be rejected")
	}
}

// The site configs are gitignored, so the revision check cannot see them; a
// local edit to one would change the pinned benchmark silently.
func TestSetupRejectsAnEditedSiteConfig(t *testing.T) {
	root := writeFixture(t)
	edited := filepath.Join(root, filepath.FromSlash("configs/global_configs.py"))
	if err := os.WriteFile(edited, []byte("podman_or_docker = 'podman'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Setup(context.Background(), root, "https://example.invalid/toolathlon.git", fixtureGitRevision(t, root))
	if err == nil || !strings.Contains(err.Error(), "differs from its pinned example") {
		t.Fatalf("err=%v", err)
	}
	if got, _ := os.ReadFile(edited); !strings.Contains(string(got), "podman") {
		t.Fatal("the edited file was overwritten rather than refused")
	}
}
