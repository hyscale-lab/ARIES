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
	"excel-only":       `{"needed_mcp_servers": ["excel", "filesystem", "terminal"], "needed_local_tools": ["claim_done", "python_execute", "sleep", "handle_overlong_tool_outputs", "manage_context", "history"], "max_turns": 10}`,
	"web-search-task":  `{"needed_mcp_servers": ["fetch"], "needed_local_tools": ["claim_done", "web_search", "python_execute"], "max_turns": 10}`,
	"odd-local-tool":   `{"needed_mcp_servers": ["memory"], "needed_local_tools": ["ai_webpage_summary"], "max_turns": 10}`,
	"object-form":      `{"needed_mcp_servers": {"memory": {"enabled": true}}, "max_turns": 10}`,
	"github-task":      `{"needed_mcp_servers": ["github", "filesystem"], "max_turns": 10}`,
	"sheets-task":      `{"needed_mcp_servers": ["google_sheet"], "max_turns": 10}`,
	"k8s-task":         `{"needed_mcp_servers": ["k8s"], "max_turns": 10}`,
	"unknown-server":   `{"needed_mcp_servers": ["mystery"], "max_turns": 10}`,
	"public-task":      `{"needed_mcp_servers": ["fetch", "scholarly", "rail_12306", "youtube-transcript", "arxiv-latex", "filesystem"], "max_turns": 10}`,
	"agent-tool-task":  `{"needed_mcp_servers": ["yahoo-finance", "web_search"], "max_turns": 10}`,
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
	writeCatalogue(t, root)
	writeFile(t, root, "scripts/formal_run_v0.json", `{"global_task_config": {"dump_path": "./dumps", "direct_to_dumps": true}}`)
	writeFile(t, root, "scripts/decoupled/container_preprocess.py", "print('preprocess')\n")
	writeFile(t, root, "utils/helper.py", "def helper(): pass\n")
	writeFile(t, root, "main.py", "print('main')\n")
	writeFile(t, root, "global_preparation/check_installation.py", "print('ok')\n")
	writeFile(t, root, "global_preparation/deploy_containers.sh", "echo not copied\n")
	writeFile(t, root, "local_binary/github-mcp-server", "binary-not-copied\n")
	writeFile(t, root, "local_binary/github-mcp-version.txt", "v0\n")
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
	// The GitHub task names its repository the way the real ones do; the
	// token itself must come from the credentials directory.
	writeFile(t, root, "tasks/"+taskPool+"/github-task/token_key_session.py", "all_token_key_session = Dict(\n    github_allowed_repos = \"Annoy-DataSync\", # only this repo\n    github_read_only = \"0\",\n)\n")
	commitFixture(t, root)
	// Ignored content that must never reach the archive.
	writeFile(t, root, "configs/__pycache__/global_configs.cpython-312.pyc", "cache\n")
	writeFile(t, root, "configs/.mcp-auth/token.json", "{}\n")
	if err := materializeSiteConfigs(root); err != nil {
		t.Fatal(err)
	}
	return root
}

// fixtureTokenKeys are the `${token.<key>}` fields a few of the fixture's
// server files substitute, as the real files do (github.yaml reads the
// token, the allowed repositories and the read-only switch from them).
var fixtureTokenKeys = map[string][]string{
	"canvas":       {"canvas_api_token", "canvas_domain"},
	"github":       {"github_token", "github_allowed_repos", "github_read_only"},
	"google_sheet": {"google_oauth2_credentials_path", "google_sheets_folder_id"},
	"huggingface":  {"huggingface_token"},
	"k8s":          {"kubeconfig_path"},
}

// writeCatalogue lays out one server file per classified server, named
// after the server as most of the real files are. The name inside is what
// counts; TestTasksChecksTheServerCatalogue covers a file named otherwise.
func writeCatalogue(t *testing.T, root string) {
	t.Helper()
	for name, kind := range serverKinds {
		if kind == serverAgentTool {
			continue
		}
		content := "name: " + name + "\ntype: stdio\nparams:\n  env:\n"
		for _, key := range fixtureTokenKeys[name] {
			content += "    " + strings.ToUpper(key) + ": \"${token." + key + "}\"\n"
		}
		writeFile(t, root, catalogueDir+"/"+name+".yaml", content)
	}
}

// writeCredentials lays out a credentials directory: Toolathlon's token file
// with the given assignments, plus any key files.
func writeCredentials(t *testing.T, assignments string, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, credentialsFileName, "from addict import Dict\nall_token_key_session = Dict(\n"+assignments+")\n")
	for _, name := range files {
		writeFile(t, dir, name, "key material\n")
	}
	return dir
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

// Tasks cite a server by the `name:` inside its file, and five files are
// named otherwise; every public server is accepted under the cited name, and
// the one agent-side tool a task lists among its servers is let through as
// Toolathlon's own runner lets it through.
func TestTasksAcceptsPublicServersByTheirCitedNames(t *testing.T) {
	root := writeFixture(t)
	options := baseOptions(t, root)
	options.TaskIDs = []string{"public-task", "agent-tool-task"}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	if servers := benchmark.details["agent-tool-task"].servers; !slices.Equal(servers, []string{"yahoo-finance", "web_search"}) {
		t.Fatalf("agent-tool-task servers = %v, want the config's list unmodified", servers)
	}
	for _, id := range options.TaskIDs {
		if benchmark.details[id].needsApplications {
			t.Fatalf("%s must not start the forwarder", id)
		}
	}
}

// A task's local tools are tools of Toolathlon's own agent loop. The
// bookkeeping ones and the terminal-shaped ones are the harness's already;
// web_search is only there when the profile enables the harness's own, and a
// tool the adapter has no mapping for is refused, like an unknown server.
func TestTasksMapsLocalToolsOntoTheHarness(t *testing.T) {
	root := writeFixture(t)
	load := func(t *testing.T, webSearch bool, ids ...string) error {
		t.Helper()
		options := baseOptions(t, root)
		options.TaskIDs = ids
		options.HarnessWebSearch = webSearch
		benchmark, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		_, err = benchmark.Tasks(context.Background())
		return err
	}
	if err := load(t, false, "excel-only", "canvas-list-test"); err != nil {
		t.Fatalf("bookkeeping and terminal tools: %v", err)
	}
	err := load(t, false, "web-search-task")
	if err == nil || !strings.Contains(err.Error(), "enable harness.web_search") {
		t.Fatalf("web_search without the harness's: err = %v", err)
	}
	if err := load(t, true, "web-search-task"); err != nil {
		t.Fatalf("web_search with the harness's: %v", err)
	}
	err = load(t, true, "odd-local-tool")
	if err == nil || !strings.Contains(err.Error(), `local tool "ai_webpage_summary" is not one the adapter maps`) {
		t.Fatalf("unknown local tool: err = %v", err)
	}
}

// Every sandbox forwards to the one application deployment, and a task's
// preprocess resets what it uses, so application-backed tasks are accepted
// only when occurrences cannot overlap.
func TestTasksRefusesApplicationTasksAboveConcurrencyOne(t *testing.T) {
	root := writeFixture(t)
	load := func(t *testing.T, concurrency int, ids ...string) error {
		t.Helper()
		options := baseOptions(t, root)
		options.TaskIDs = ids
		options.Concurrency = concurrency
		benchmark, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		_, err = benchmark.Tasks(context.Background())
		return err
	}
	if err := load(t, 1, "canvas-list-test", "excel-only"); err != nil {
		t.Fatalf("concurrency 1: %v", err)
	}
	if err := load(t, 4, "excel-only", "object-form"); err != nil {
		t.Fatalf("local-only tasks at concurrency 4: %v", err)
	}
	err := load(t, 2, "excel-only", "canvas-list-test")
	if err == nil || !strings.Contains(err.Error(), "execution.concurrency 1 (concurrency 2 with canvas-list-test)") {
		t.Fatalf("application task at concurrency 2: err = %v", err)
	}
	options := baseOptions(t, root)
	options.TaskIDs = []string{"excel-only", "github-task"}
	options.Concurrency = 2
	options.CredentialsDir = writeCredentials(t, "    github_token = \"ghp_example\",\n")
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "execution.concurrency 1 (concurrency 2 with github-task)") {
		t.Fatalf("account task at concurrency 2: err = %v", err)
	}
	options = baseOptions(t, root)
	options.Concurrency = -1
	if _, err := New(options); err == nil || !strings.Contains(err.Error(), "concurrency must be positive") {
		t.Fatalf("negative concurrency: err = %v", err)
	}
}

// The classification is only as good as its agreement with the pinned
// checkout, so task load compares the two and names any difference.
func TestTasksChecksTheServerCatalogue(t *testing.T) {
	run := func(t *testing.T, mutate func(root string)) error {
		t.Helper()
		root := writeFixture(t)
		mutate(root)
		commitFixture(t, root)
		options := baseOptions(t, root)
		options.TaskIDs = []string{"excel-only"}
		benchmark, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		_, err = benchmark.Tasks(context.Background())
		return err
	}
	if err := run(t, func(root string) {
		// The file name is not the server name: the real npx-fetch.yaml.
		if err := os.Rename(filepath.Join(root, catalogueDir, "fetch.yaml"), filepath.Join(root, catalogueDir, "npx-fetch.yaml")); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatalf("renamed file: %v", err)
	}
	err := run(t, func(root string) {
		writeFile(t, root, catalogueDir+"/xmind.yaml", "name: xmind\ntype: stdio\n")
		if err := os.Remove(filepath.Join(root, catalogueDir, "git.yaml")); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"does not classify (xmind)", "lacks servers it expects (git)"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	err = run(t, func(root string) {
		writeFile(t, root, catalogueDir+"/git.yaml", "type: stdio\n")
	})
	if err == nil || !strings.Contains(err.Error(), "git.yaml has no top-level name") {
		t.Fatalf("err = %v, want the nameless file named", err)
	}
}

func TestTasksRejectsTasksTheSandboxCannotServe(t *testing.T) {
	root := writeFixture(t)
	cases := map[string]string{
		"github-task":     "third-party account: set benchmark.toolathlon.credentials_dir",
		"k8s-task":        "host runtime the sandbox does not provide",
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

// The runner's occurrence ID is the logical ID plus "-NNN", so a logical ID
// at its own 128-byte limit yields a 132-byte execution ID: that must pass,
// and only the execution ID's own 149-byte limit may refuse a longer one.
func TestSafeExecutionTaskIDHasItsOwnLengthLimit(t *testing.T) {
	longest := strings.Repeat("a", 128)
	if !safeExecutionTaskID(longest, longest+"-001") {
		t.Fatal("128-byte logical ID with an occurrence suffix rejected")
	}
	atLimit := strings.Repeat("b", 145)
	if !safeExecutionTaskID(atLimit, atLimit+"-001") {
		t.Fatal("149-byte execution ID rejected")
	}
	over := strings.Repeat("c", 146)
	if safeExecutionTaskID(over, over+"-001") {
		t.Fatal("150-byte execution ID accepted")
	}
	for _, bad := range []string{"task", "task-", "task-0", "task-01", "task-abc", "task-001/x", "other-001", "task-1000000000000000000000"} {
		if safeExecutionTaskID("task", bad) {
			t.Fatalf("%q accepted", bad)
		}
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
		"empty host label": func(o *Options) { o.AppHost = "host..example" },
		"hyphen-edged host label": func(o *Options) {
			o.AppHost = "host-.example"
		},
		"leading hyphen host": func(o *Options) { o.AppHost = "-host" },
		"negative steps":      func(o *Options) { o.MaxSteps = -1 },
		"bad model name":      func(o *Options) { o.ModelName = "deep seek" },
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

// An account-backed task loads when the credentials directory provides
// every field its server reads; the check names what is missing, the task's
// own token file counts, and the k8s server stays refused.
func TestTasksRunsAccountTasksWithCredentials(t *testing.T) {
	root := writeFixture(t)
	load := func(t *testing.T, dir, id string) (taskDetails, error) {
		t.Helper()
		options := baseOptions(t, root)
		options.TaskIDs = []string{id}
		options.CredentialsDir = dir
		benchmark, err := New(options)
		if err != nil {
			return taskDetails{}, err
		}
		if _, err := benchmark.Tasks(context.Background()); err != nil {
			return taskDetails{}, err
		}
		return benchmark.details[id], nil
	}
	details, err := load(t, writeCredentials(t, "    github_token = \"ghp_example\", # filled\n    huggingface_token = \"XX\", # not this task's\n"), "github-task")
	if err != nil {
		t.Fatalf("filled token: %v", err)
	}
	if !details.needsCredentials || !slices.Equal(details.extraEntries, []string{"local_binary/github-mcp-server"}) {
		t.Fatalf("details = %+v: the task must carry the credentials and the server binary", details)
	}
	if details, err := load(t, writeCredentials(t, "    github_token = \"ghp_example\",\n"), "excel-only"); err != nil || details.needsCredentials || details.extraEntries != nil {
		t.Fatalf("a task without an account server must not carry credentials: %+v, %v", details, err)
	}

	refusals := map[string]struct {
		assignments string
		files       []string
		id          string
		want        string
	}{
		"placeholder":   {"    github_token = \"XX\", # TO BE FILLED\n", nil, "github-task", "github_token (still the example's placeholder)"},
		"not set":       {"    huggingface_token = \"hf_example\",\n", nil, "github-task", "github_token (not set)"},
		"missing file":  {"    google_oauth2_credentials_path = \"configs/google_credentials.json\",\n    google_sheets_folder_id = \"XX\",\n", nil, "sheets-task", "google_oauth2_credentials_path (file configs/google_credentials.json is not in the directory), google_sheets_folder_id (still the example's placeholder)"},
		"escaping path": {"    google_oauth2_credentials_path = \"configs/../../etc/passwd\",\n    google_sheets_folder_id = \"1abc\",\n", nil, "sheets-task", "google_oauth2_credentials_path (not a file under configs/)"},
		"k8s":           {"    github_token = \"ghp_example\",\n", nil, "k8s-task", "host runtime the sandbox does not provide"},
	}
	for name, testCase := range refusals {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, writeCredentials(t, testCase.assignments, testCase.files...), testCase.id)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("err = %v, want it to contain %q", err, testCase.want)
			}
		})
	}
	// The sheets task, with the key file present and a computed value for
	// a field the example derives from that file.
	dir := writeCredentials(t, "    google_oauth2_credentials_path = \"configs/google_credentials.json\",\n    google_sheets_folder_id = credentials.get(\"folder\", \"\"),\n", "google_credentials.json")
	if details, err := load(t, dir, "sheets-task"); err != nil || !details.needsCredentials || details.extraEntries != nil {
		t.Fatalf("sheets task with its key file: %+v, %v", details, err)
	}

	// The directory itself is checked when the benchmark is built.
	options := baseOptions(t, root)
	options.CredentialsDir = filepath.Join(t.TempDir(), "absent")
	if _, err := New(options); err == nil || !strings.Contains(err.Error(), "credentials directory") {
		t.Fatalf("absent directory: err = %v", err)
	}
	options.CredentialsDir = t.TempDir()
	if _, err := New(options); err == nil || !strings.Contains(err.Error(), "must hold Toolathlon's filled token_key_session.py") {
		t.Fatalf("directory without the token file: err = %v", err)
	}
}

func TestParseTokenAssignmentsReadsTheExampleShape(t *testing.T) {
	values := parseTokenAssignments([]byte(`from addict import Dict
if os.path.exists("./configs/google_credentials.json"):
    google_credentials_filename = "./configs/google_credentials.json"
all_token_key_session = Dict(
    timezone = "Asia/Hong_Kong",
    serper_api_key = "XX", # TO BE FILLED, you can fill in multiple keys separated by comma
    google_client_id = google_credentials.get("client_id", ""),
    github_read_only = "1", # default to ban write
    canvas_domain = 'localhost:20001',
    snowflake_private_key_path = snowflake_private_key_path, # TO BE FILLED
)
`))
	for key, want := range map[string]tokenValue{
		"timezone":                    {literal: "Asia/Hong_Kong", isLiteral: true},
		"serper_api_key":              {literal: "XX", isLiteral: true},
		"google_client_id":            {},
		"github_read_only":            {literal: "1", isLiteral: true},
		"canvas_domain":               {literal: "localhost:20001", isLiteral: true},
		"snowflake_private_key_path":  {},
		"google_credentials_filename": {literal: "./configs/google_credentials.json", isLiteral: true},
	} {
		if got, ok := values[key]; !ok || got != want {
			t.Fatalf("%s = %+v (present %v), want %+v", key, got, ok, want)
		}
	}
	if _, present := values["if os"]; present {
		t.Fatal("a statement was read as an assignment")
	}
}

func TestWriteCredentialsArchiveOverlaysConfigs(t *testing.T) {
	dir := writeCredentials(t, "    github_token = \"ghp_example\",\n", "google_credentials.json", ".mcp-auth/notion.json")
	writeFile(t, dir, "__pycache__/token_key_session.cpython-312.pyc", "cache\n")
	archive := filepath.Join(t.TempDir(), "credentials.tar")
	if err := writeCredentialsArchive(dir, archive); err != nil {
		t.Fatal(err)
	}
	members, err := archiveMemberNames(archive)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(members)
	want := []string{"configs/.mcp-auth/", "configs/.mcp-auth/notion.json", "configs/google_credentials.json", "configs/token_key_session.py"}
	if !slices.Equal(members, want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
	if err := os.Symlink(filepath.Join(dir, "google_credentials.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := writeCredentialsArchive(dir, archive); err == nil || !strings.Contains(err.Error(), "neither a regular file nor a directory") {
		t.Fatalf("symlink: err = %v", err)
	}
}
