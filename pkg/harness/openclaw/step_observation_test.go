package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hyscale-lab/aries/pkg/core"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/harness"
)

func TestStepObservationPreservesNativeConfigAndStagesOnlyPlugin(t *testing.T) {
	original := []byte(`{"models":{"providers":{"aries":{"apiKey":"${MODEL_KEY}"}}},"plugins":{"entries":{"searxng":{"enabled":true}}},"tools":{"deny":["read"]}}`)
	rendered, err := renderStepObservation(original)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	_ = json.Unmarshal(original, &before)
	_ = json.Unmarshal(rendered, &after)
	for _, key := range []string{"models", "tools"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("native %s changed", key)
		}
	}
	plugins := after["plugins"].(map[string]any)
	entries := plugins["entries"].(map[string]any)
	if entries["searxng"] == nil || entries["aries-step-pages"] == nil {
		t.Fatalf("entries not merged: %v", entries)
	}
	observer := entries["aries-step-pages"].(map[string]any)
	hooks := observer["hooks"].(map[string]any)
	if hooks["allowConversationAccess"] != true {
		t.Fatal("native terminal-release hooks would be blocked by OpenClaw")
	}
	dir := t.TempDir()
	for _, name := range []string{"index.mjs", "end-request.mjs", "openclaw.plugin.json", "package.json", "ignore-private.key"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]harness.ArchiveFile{}
	if err = stageStepObservation(files, dir); err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("staged %d files", len(files))
	}
	for name, file := range files {
		if strings.Contains(name, "private") || file.Mode != 0400 {
			t.Fatalf("unexpected staged file %s: %v", name, file.Mode)
		}
	}
	if err = os.Remove(filepath.Join(dir, "index.mjs")); err != nil {
		t.Fatal(err)
	}
	if err = stageStepObservation(map[string]harness.ArchiveFile{}, dir); err == nil {
		t.Fatal("missing runtime plugin accepted")
	}
}
func TestStepObservationIdentityDerivedFromOccurrence(t *testing.T) {
	o := &StepObservationOptions{ObserverURL: "http://172.17.0.1:9100", PluginDir: "/private/plugin"}
	a := &harness.Occurrence{RunID: "run", TaskID: "task", AttemptID: "attempt"}
	env := stepObservationEnvironment(o, a)
	if !strings.Contains(strings.Join(env, "\n"), `ARIES_STEP_SESSION_ID=["run","task","attempt"]`) {
		t.Fatalf("identity=%v", env)
	}
	a.AttemptID = "retry"
	if reflect.DeepEqual(env, stepObservationEnvironment(o, a)) {
		t.Fatal("retry reused session identity")
	}
	for _, mutation := range []func(*StepObservationOptions){func(o *StepObservationOptions) { o.ObserverURL = "http://secret@localhost/" }, func(o *StepObservationOptions) { o.ObserverURL = "http://localhost/?key=value" }, func(o *StepObservationOptions) { o.PluginDir = "relative" }} {
		bad := *o
		mutation(&bad)
		if err := bad.validate(ModeAgent); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	if err := o.validate(ModeRealtime); err == nil {
		t.Fatal("non-agent mode accepted")
	}
}

func TestStepObservationNativeLifecycleStaging(t *testing.T) {
	fake := newFakeDeployment()
	m := newTestManager(t, fake, []byte("model-secret"))
	dir := t.TempDir()
	for _, name := range []string{"index.mjs", "end-request.mjs", "openclaw.plugin.json", "package.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["ok"] = true
		body["released"] = true
		body["paused"] = false
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	m.options.StepObservation = &StepObservationOptions{PluginDir: dir, ObserverURL: server.URL}
	req := core.HarnessRequest{Connectivity: core.HarnessConnectivity{SearchURL: "http://search.example:8123", Placement: core.RuntimePlacement{AttachmentID: "aries-net-test"}}, RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := m.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	files := readArchive(t, fake.archive)
	for _, name := range []string{"index.mjs", "end-request.mjs", "openclaw.plugin.json", "package.json"} {
		file, ok := files["run/aries/step-pages/"+name]
		if !ok || file.uid != 1000 || file.gid != 1000 || file.mode != 0400 || string(file.content) != name {
			t.Fatalf("plugin not staged correctly: %s %+v", name, file)
		}
	}
	env := strings.Join(fake.created.Env, "\n")
	if !strings.Contains(env, `ARIES_STEP_SESSION_ID=["run-1","fix-git","attempt"]`) || !strings.Contains(env, "ARIES_STEP_TURN_ID=1") || strings.Contains(env, "model-secret") {
		t.Fatalf("invalid observation env: %s", env)
	}
	var cfg map[string]any
	if err := json.Unmarshal(files["run/aries/openclaw.json"].content, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["plugins"].(map[string]any)["entries"].(map[string]any)["aries-step-pages"] == nil {
		t.Fatal("plugin not enabled")
	}
}

func TestObserverReleaseAfterRuntimeRemovalRetriesWithoutAnotherRemoval(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !fake.removed {
			t.Error("observer release preceded confirmed runtime removal")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/session/abort" || body["terminal_complete"] != false {
			t.Error("not cancellation barrier")
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		body["ok"] = true
		body["released"] = true
		body["paused"] = false
		body["measurement_valid"] = false
		body["terminal_complete"] = false
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	manager.options.StepObservation = &StepObservationOptions{ObserverURL: server.URL}
	active := &session{Occurrence: &harness.Occurrence{ID: "owned", RunID: "r", TaskID: "t", AttemptID: "a", ArtifactDir: t.TempDir()}}
	manager.active = active
	if err := manager.runtime.Own(active.Occurrence); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Stop(ctx); err == nil {
		t.Fatal("unconfirmed release accepted")
	}
	if manager.active == nil {
		t.Fatal("unconfirmed observer ownership lost")
	}
	if err := manager.Start(context.Background(), core.HarnessRequest{}); err == nil {
		t.Fatal("new admission while release unresolved")
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.stopCalls != 1 || calls != 2 || manager.active != nil {
		t.Fatalf("remove=%d release=%d active=%v", fake.stopCalls, calls, manager.active)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("idempotent stop repeated observer cleanup")
	}
	paths, _ := filepath.Glob(filepath.Join(active.ArtifactDir, "observer-release-*.json"))
	if len(paths) != 2 {
		t.Fatalf("retry evidence overwritten: %v", paths)
	}
}
func TestObserverReleaseRejectsAmbiguousOrWrongSessionACK(t *testing.T) {
	for _, reply := range []string{
		`{"ok":true,"released":true,"paused":false,"session_id":"wrong","turn_id":"1"}`,
		`{"ok":true,"paused":false,"session_id":"[\"r\",\"t\",\"a\"]","turn_id":"1"}`,
		`{"ok":true,"released":true,"paused":true,"session_id":"[\"r\",\"t\",\"a\"]","turn_id":"1"}`,
	} {
		t.Run(reply, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(reply)) }))
			defer server.Close()
			manager := &Manager{options: Options{StepObservation: &StepObservationOptions{ObserverURL: server.URL}}}
			active := &session{Occurrence: &harness.Occurrence{RunID: "r", TaskID: "t", AttemptID: "a", ArtifactDir: t.TempDir()}}
			if err := manager.releaseStepObservation(context.Background(), active); err == nil {
				t.Fatal("ambiguous ACK accepted")
			}
		})
	}
}
func TestObserverReleaseRespectsBoundedContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer server.Close()
	manager := &Manager{options: Options{StepObservation: &StepObservationOptions{ObserverURL: server.URL}}}
	active := &session{Occurrence: &harness.Occurrence{RunID: "r", TaskID: "t", AttemptID: "a", ArtifactDir: t.TempDir()}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.releaseStepObservation(ctx, active); err == nil || errors.Is(err, nil) {
		t.Fatal("missing response accepted")
	}
}
