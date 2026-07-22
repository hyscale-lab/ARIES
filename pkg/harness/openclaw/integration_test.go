//go:build integration

package openclaw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const integrationAPIKeyEnv = "ARIES_OPENCLAW_INTEGRATION_KEY"

// modelBridge starts the deterministic model on the task network after the
// real SSH bridge has made that network available. Runner ordering then stops
// OpenClaw before this bridge removes the model and revokes SSH.
type modelBridge struct {
	inner *openclawssh.Manager
	api   *client.Client
	runID string
	key   string
	image string
	id    string
}

func (bridge *modelBridge) Start(ctx context.Context, sandbox runner.Sandbox) (core.ToolEndpoint, error) {
	endpoint, err := bridge.inner.Start(ctx, sandbox)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	digest := sha256.Sum256([]byte(bridge.key))
	created, err := bridge.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "aries-fake-model-" + bridge.runID,
		Config: &container.Config{
			Image: bridge.image, Entrypoint: []string{"node"},
			Cmd:    []string{"-e", deterministicModelScript(), hex.EncodeToString(digest[:])},
			Labels: map[string]string{"aries.managed": "true", "aries.kind": "fake-model", "aries.run": bridge.runID},
		},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(endpoint.Network)},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			endpoint.Network: {Aliases: []string{"fake-model"}},
		}},
	})
	if err == nil {
		bridge.id = created.ID
		_, err = bridge.api.ContainerStart(ctx, bridge.id, client.ContainerStartOptions{})
	}
	if err != nil {
		_ = bridge.removeModel(context.WithoutCancel(ctx))
		return core.ToolEndpoint{}, errors.Join(err, bridge.inner.Stop(context.WithoutCancel(ctx)))
	}
	return endpoint, nil
}

func (bridge *modelBridge) Stop(ctx context.Context) error {
	return errors.Join(bridge.removeModel(ctx), bridge.inner.Stop(ctx))
}

func (bridge *modelBridge) removeModel(ctx context.Context) error {
	if bridge.id == "" {
		return nil
	}
	_, err := bridge.api.ContainerRemove(ctx, bridge.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if errdefs.IsNotFound(err) {
		err = nil
	}
	if err == nil {
		bridge.id = ""
	}
	return err
}

func TestRunnerFixGitThroughOpenClawSSHBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	api, err := client.New(client.FromEnv, client.WithUserAgent("aries-openclaw-integration/1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon is required: %v", err)
	}
	root := repositoryRoot(t)
	versions, err := config.LoadVersions(filepath.Join(root, "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	datasetRoot := filepath.Join(root, terminalbench.DefaultRoot)
	benchmarkOptions := terminalbench.Options{
		Root: datasetRoot, TaskIDs: []string{"fix-git"}, OutputDir: t.TempDir(),
		Revision: versions.TerminalBench2.Revision, FixGitImage: versions.TerminalBench2.FixGitImage,
	}
	benchmark, err := terminalbench.New(benchmarkOptions)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("load fix-git = %#v, %v", tasks, err)
	}
	ensureSDKImage(t, ctx, api, versions.OpenClaw.Image)
	ensureSDKImage(t, ctx, api, tasks[0].Environment.Image)

	outputDir := t.TempDir()
	runID := "openclaw-sdk-fix-git"
	key := "deterministic-integration-key"
	t.Setenv(integrationAPIKeyEnv, key)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sandbox, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, CleanupTimeout: 30 * time.Second, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	sshBridge, err := openclawssh.New(openclawssh.Options{OutputDir: outputDir, ClientPath: requiredIntegrationFile(t, "ARIES_SSH_CLIENT"), CleanupTimeout: 30 * time.Second, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	bridge := &modelBridge{inner: sshBridge, api: api, runID: runID, key: key, image: versions.OpenClaw.Image}
	harness, err := New(Options{Image: versions.OpenClaw.Image, OutputDir: outputDir, CleanupTimeout: 30 * time.Second, StartTimeout: 60 * time.Second, AgentTimeout: 3 * time.Minute, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	benchmarkOptions.OutputDir = outputDir
	benchmark, err = terminalbench.New(benchmarkOptions)
	if err != nil {
		t.Fatal(err)
	}
	resourceMonitor, err := monitor.New(monitor.Options{
		RunID: runID, TaskIDs: []string{"fix-git"}, OutputDir: outputDir,
		Interval: time.Second, StopTimeout: 20 * time.Second, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	composed, err := runner.New(benchmark, harness, sandbox, bridge, runner.Options{
		Name: "openclaw-tb2-fix-git-deterministic", RunID: runID, OutputDir: outputDir,
		Model:          core.ModelConfig{BaseURL: "http://fake-model:8080/v1", Model: "aries-deterministic", APIKeyEnv: integrationAPIKeyEnv},
		CleanupTimeout: 45 * time.Second, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cleanupCancel()
		_ = harness.Stop(cleanupCtx)
		_ = bridge.Stop(cleanupCtx)
	})
	if err := resourceMonitor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := composed.Run(ctx)
	monitorCtx, monitorCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	reports, monitorErr := resourceMonitor.Stop(monitorCtx)
	monitorCancel()
	if err != nil {
		t.Fatalf("Runner = %#v, %v", result, err)
	}
	if monitorErr != nil {
		t.Fatal(monitorErr)
	}
	report := reports["fix-git"]
	if report.Status != core.StatusSucceeded || report.SampleCount == 0 || len(report.LogPaths) == 0 {
		t.Fatalf("monitor report = %#v", report)
	}
	assertResourceCoverage(t, report, runID, "fix-git")
	if len(result.Tasks) != 1 {
		t.Fatalf("task results = %#v", result.Tasks)
	}
	task := result.Tasks[0]
	if task.Harness.Status != core.StatusSucceeded || task.Evaluation.Status != core.StatusSucceeded || task.Evaluation.Reward != 1 ||
		task.Isolation.Status != core.StatusConfirmed || !task.Isolation.HarnessStopped || !task.Isolation.BridgeRevoked || task.Cleanup.Status != core.StatusSucceeded {
		t.Fatalf("separate outcomes = %#v", task)
	}
	if len(task.ToolLogPaths) != 1 {
		t.Fatalf("tool logs = %q", task.ToolLogPaths)
	}
	toolCalls, err := os.ReadFile(task.ToolLogPaths[0])
	if err != nil || bytes.Count(toolCalls, []byte(`"status":"completed"`)) < 4 {
		t.Fatalf("bridge tool calls = %q, %v", toolCalls, err)
	}
	if !strings.Contains(task.Harness.FinalResponse, "Recovered lost commit") {
		t.Fatalf("final response = %q", task.Harness.FinalResponse)
	}
	assertNoRunResources(t, ctx, api, runID)
	assertSecretAbsent(t, outputDir, key)
}

func assertResourceCoverage(t *testing.T, report core.ObserverResult, runID, taskID string) {
	t.Helper()
	var indexPath string
	for _, path := range report.LogPaths {
		if filepath.Base(path) == "index.json" {
			indexPath = path
			break
		}
	}
	if indexPath == "" {
		t.Fatalf("monitor report has no index: %#v", report)
	}
	content, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read monitor index: %v", err)
	}
	var index monitor.Index
	if err := json.Unmarshal(content, &index); err != nil {
		t.Fatalf("decode monitor index: %v", err)
	}
	if index.RunID != runID || index.TaskID != taskID {
		t.Fatalf("monitor identity = run %q task %q", index.RunID, index.TaskID)
	}
	coverage := make(map[string]uint64, len(index.Components))
	for _, component := range index.Components {
		coverage[component.Component] += component.SampleCount
	}
	if coverage["task-container"] == 0 || coverage["openclaw-harness"] == 0 {
		t.Fatalf("monitor coverage = %#v", coverage)
	}
}

func ensureSDKImage(t *testing.T, ctx context.Context, api *client.Client, image string) {
	t.Helper()
	if _, err := api.ImageInspect(ctx, image); err == nil {
		return
	} else if !errdefs.IsNotFound(err) {
		t.Fatal(err)
	}
	pull, err := api.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull %s: %v", image, err)
	}
	defer pull.Close()
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull %s: %v", image, err)
	}
}

func assertNoRunResources(t *testing.T, ctx context.Context, api *client.Client, runID string) {
	t.Helper()
	filters := client.Filters{}.Add("label", "aries.run="+runID)
	containers, err := api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil || len(containers.Items) != 0 {
		t.Fatalf("leaked containers = %#v, %v", containers.Items, err)
	}
	networks, err := api.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil || len(networks.Items) != 0 {
		t.Fatalf("leaked networks = %#v, %v", networks.Items, err)
	}
}

func assertSecretAbsent(t *testing.T, root, secret string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(secret)) {
			return fmt.Errorf("secret persisted in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("repository root was not found")
		}
		current = parent
	}
}

func requiredIntegrationFile(t *testing.T, environment string) string {
	t.Helper()
	path, err := filepath.Abs(os.Getenv(environment))
	if err != nil || path == "" {
		t.Fatalf("%s must name a built helper: %v", environment, err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s = %q is not a file: %v", environment, path, err)
	}
	return path
}

func deterministicModelScript() string {
	return `const http=require("http"),crypto=require("crypto");
const expected=process.argv[1];let step=0,candidate="",previous={};
function text(content){return typeof content==="string"?content:Array.isArray(content)?content.map(x=>x&&x.text||"").join("\n"):JSON.stringify(content)}
function prior(body,id,command){const ms=body.messages||[],a=[...ms].reverse().find(m=>m.role==="assistant"&&Array.isArray(m.tool_calls)),r=[...ms].reverse().find(m=>m.role==="tool");if(!a||!r||a.tool_calls[0].id!==id||r.tool_call_id!==id)throw Error("tool chain mismatch");if(JSON.parse(a.tool_calls[0].function.arguments).command!==command)throw Error("command mismatch");return text(r.content)}
function stream(res,delta,finish){const id="aries-"+step;res.writeHead(200,{"content-type":"text/event-stream","cache-control":"no-cache","connection":"close"});res.write("data: "+JSON.stringify({id,object:"chat.completion.chunk",created:1,model:"aries-deterministic",choices:[{index:0,delta,finish_reason:null}]})+"\n\n");res.write("data: "+JSON.stringify({id,object:"chat.completion.chunk",created:1,model:"aries-deterministic",choices:[{index:0,delta:{},finish_reason:finish}]})+"\n\n");res.end("data: [DONE]\n\n")}
function call(res,id,command){previous={id,command};stream(res,{role:"assistant",tool_calls:[{index:0,id,type:"function",function:{name:"exec",arguments:JSON.stringify({command})}}]},"tool_calls")}
const status="printf '%s\\n' ARIES_STATUS; git status --short --branch; printf '%s\\n' ARIES_HEAD; git rev-parse HEAD; printf '%s\\n' ARIES_REFLOG; git reflog --all --format='%H %gs' -20";
http.createServer((req,res)=>{let raw="";req.on("data",c=>raw+=c);req.on("end",()=>{try{if(req.method!=="POST"||req.url!=="/v1/chat/completions")throw Error("route");const bearer=(req.headers.authorization||"").replace(/^Bearer /,"");if(crypto.createHash("sha256").update(bearer).digest("hex")!==expected)throw Error("auth");const body=JSON.parse(raw);if(body.model!=="aries-deterministic"||body.stream!==true)throw Error("request");
if(step===0){step++;return call(res,"status",status)}
const out=prior(body,previous.id,previous.command);
if(step===1){const head=(out.match(/ARIES_HEAD\s+([0-9a-f]{40})/)||[])[1],hashes=[...out.matchAll(/\b[0-9a-f]{40}\b/g)].map(x=>x[0]);candidate=hashes.find(x=>x!==head)||"";if(!candidate)throw Error("candidate");step++;return call(res,"inspect","git show --format=fuller --stat "+candidate+" && git branch --contains "+candidate)}
if(step===2){if(!out.includes(candidate.slice(0,7)))throw Error("inspect");step++;return call(res,"merge","git checkout master && git -c user.name='ARIES Benchmark' -c user.email='aries@example.invalid' merge -X theirs --no-edit "+candidate)}
if(step===3){if(!/fast-forward|merge made|already up.to.date/i.test(out))throw Error("merge");step++;return call(res,"verify","git merge-base --is-ancestor "+candidate+" HEAD && test -z \"$(git status --porcelain)\" && git status --short --branch && git log --oneline -5")}
if(step===4){if(!out.includes(candidate.slice(0,7))||!out.includes("master"))throw Error("verify");step++;return stream(res,{role:"assistant",content:"Recovered lost commit "+candidate+" and verified a clean master branch."},"stop")}
throw Error("extra request")}catch(error){res.writeHead(400,{"content-type":"application/json"});res.end(JSON.stringify({error:{message:error.message}}))}})}).listen(8080,"0.0.0.0");`
}
