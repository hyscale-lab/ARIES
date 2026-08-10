package openclaw

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type pinnedPluginContract struct {
	Upstream struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		TagObject  string `json:"tagObject"`
		Commit     string `json:"commit"`
	} `json:"upstream"`
	Sources map[string]string `json:"sources"`
	Plugin  struct {
		SDKImport            string `json:"sdkImport"`
		RegistrationFunction string `json:"registrationFunction"`
		RegistrationMode     string `json:"registrationMode"`
		BackendID            string `json:"backendID"`
		LocalLoadPath        string `json:"localLoadPath"`
		Manifest             string `json:"manifest"`
		EntrypointField      string `json:"entrypointField"`
		Config               struct {
			Enabled         bool     `json:"plugins.enabled"`
			Allow           []string `json:"plugins.allow"`
			LoadPaths       []string `json:"plugins.load.paths"`
			EntryEnabled    bool     `json:"plugins.entries.aries-e2b.enabled"`
			Backend         string   `json:"agents.defaults.sandbox.backend"`
			Mode            string   `json:"agents.defaults.sandbox.mode"`
			Scope           string   `json:"agents.defaults.sandbox.scope"`
			WorkspaceAccess string   `json:"agents.defaults.sandbox.workspaceAccess"`
		} `json:"config"`
	} `json:"plugin"`
	Backend struct {
		CreateParams          []string `json:"createParams"`
		RequiredHandleFields  []string `json:"requiredHandleFields"`
		ExecSpecFields        []string `json:"execSpecFields"`
		BuildExecSpecParams   []string `json:"buildExecSpecParams"`
		CommandParams         []string `json:"commandParams"`
		ManagerIsOptional     bool     `json:"managerIsOptional"`
		AriesRegistersManager bool     `json:"ariesRegistersManager"`
	} `json:"backend"`
	FilesystemBridge struct {
		Methods     []string `json:"methods"`
		HasListDir  bool     `json:"hasListDir"`
		NativeTools struct {
			Enabled                      []string `json:"enabled"`
			Disabled                     []string `json:"disabled"`
			WriteRequiresWorkspaceAccess string   `json:"writeRequiresWorkspaceAccess"`
		} `json:"nativeTools"`
	} `json:"filesystemBridge"`
	Secrets struct {
		TokenConfigField             string `json:"tokenConfigField"`
		TokenFile                    string `json:"tokenFile"`
		TokenBytesInOpenClawJSON     bool   `json:"tokenBytesInOpenClawJSON"`
		PluginEntrySupportsSecretRef bool   `json:"pluginEntrySupportsSecretRef"`
	} `json:"secrets"`
	Limitations struct {
		BuiltInE2BBackend                 bool `json:"builtInE2BBackend"`
		DirectProcessStartMethod          bool `json:"directProcessStartMethod"`
		DirectProcessSendSignalMethod     bool `json:"directProcessSendSignalMethod"`
		BuildExecSpecAcceptsCommandString bool `json:"buildExecSpecAcceptsCommandString"`
		FilesystemBridgeHasListDir        bool `json:"filesystemBridgeHasListDir"`
	} `json:"limitations"`
}

func TestPinnedOpenClaw202671SandboxPluginContract(t *testing.T) {
	content, err := os.ReadFile("testdata/v2026.7.1-sandbox-plugin-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract pinnedPluginContract
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		t.Fatal(err)
	}

	if contract.Upstream.Repository != "https://github.com/openclaw/openclaw" || contract.Upstream.Tag != "v2026.7.1" || contract.Upstream.TagObject != "842a951d5d0843aa6eb77575dc9867bf0603835c" || contract.Upstream.Commit != "2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4" {
		t.Fatalf("upstream pin = %#v", contract.Upstream)
	}
	wantSources := map[string]string{
		"src/plugin-sdk/sandbox.ts":                  "3140345cf1e88d1881aee3b72f2b3a4d6bd01598f8faab26eef20b9d64caa251",
		"src/agents/sandbox/backend.ts":              "22f8fc3ec6e2494839908588460040d46bbaa71124f5d46b8be71f810f73d577",
		"src/agents/sandbox/backend.types.ts":        "ab98d85f2d11a4d06c688b395b05afce8ea434062fb5718e58af7a17c7257b35",
		"src/agents/sandbox/backend-handle.types.ts": "5c1c5a950e0868fad40095623f01ac0e50e679830ba19469b7ea0cc734a12a29",
		"src/agents/sandbox/fs-bridge.types.ts":      "6316c649cad175874fc91aa5d63455077b9db4dae95f724838fbeb9c1bcf603b",
		"src/agents/sandbox/context.ts":              "b69df7104620bcb0e9b8bf1a29649dd0d42441ec8db1a44cf26d6321b36fe2a6",
		"src/config/types.plugins.ts":                "82dbcaef080dbf0ee70519a583a7bcab690f9a21acca34367c82a9d3ba7e95f0",
		"src/config/types.agents-shared.ts":          "bc70f812f37f8ec041f8e86da466ad0f62b8f4a40f7175b954748b6d42012271",
		"src/config/zod-schema.agent-runtime.ts":     "041d8ca8d94d10b5585b95f707fddef6da4e9f7b42f203e7695f77f826492d3e",
		"src/agents/agent-tools.ts":                  "e3525593ff35dad9a996dbc8f029b8d8459cd03c5dfd468f8f34e5b09885d48a",
		"extensions/openshell/index.ts":              "cdf7bbfb1842dafbda7b38bf160e683f73489c193d65cf60b8a828e5de1e9ce9",
		"extensions/openshell/openclaw.plugin.json":  "c7cd076727e86dbcfd2869ef3936c430116b0f15240cb7b1837ff97cdd38be1a",
		"extensions/openshell/package.json":          "1dbe9d8fe95859289b08dc582f78c07a44476db061ef53f0cea6fbafefa12158",
	}
	if !reflect.DeepEqual(contract.Sources, wantSources) {
		t.Fatalf("pinned sources = %#v", contract.Sources)
	}
	if contract.Plugin.SDKImport != "openclaw/plugin-sdk/sandbox" || contract.Plugin.RegistrationFunction != "registerSandboxBackend" || contract.Plugin.RegistrationMode != "full" || contract.Plugin.BackendID != "aries-e2b" {
		t.Fatalf("plugin seam = %#v", contract.Plugin)
	}
	if !contract.Plugin.Config.Enabled || !reflect.DeepEqual(contract.Plugin.Config.Allow, []string{"aries-e2b"}) || !reflect.DeepEqual(contract.Plugin.Config.LoadPaths, []string{"/opt/aries/openclaw/aries-e2b"}) || !contract.Plugin.Config.EntryEnabled || contract.Plugin.Config.Backend != "aries-e2b" || contract.Plugin.Config.Mode != "all" || contract.Plugin.Config.Scope != "shared" || contract.Plugin.Config.WorkspaceAccess != "rw" {
		t.Fatalf("plugin selection = %#v", contract.Plugin.Config)
	}
	if !reflect.DeepEqual(contract.Backend.CreateParams, []string{"sessionKey", "scopeKey", "workspaceDir", "agentWorkspaceDir", "skillsWorkspaceDir", "cfg"}) || !reflect.DeepEqual(contract.Backend.RequiredHandleFields, []string{"id", "runtimeId", "runtimeLabel", "workdir", "buildExecSpec", "runShellCommand"}) || !reflect.DeepEqual(contract.Backend.ExecSpecFields, []string{"argv", "env", "stdinMode", "finalizeToken"}) || !reflect.DeepEqual(contract.Backend.BuildExecSpecParams, []string{"command", "workdir", "env", "usePty"}) || !reflect.DeepEqual(contract.Backend.CommandParams, []string{"script", "args", "stdin", "allowFailure", "signal"}) || !contract.Backend.ManagerIsOptional || contract.Backend.AriesRegistersManager {
		t.Fatalf("backend contract = %#v", contract.Backend)
	}
	if !reflect.DeepEqual(contract.FilesystemBridge.Methods, []string{"resolvePath", "readFile", "writeFile", "mkdirp", "remove", "rename", "stat"}) || contract.FilesystemBridge.HasListDir || !reflect.DeepEqual(contract.FilesystemBridge.NativeTools.Enabled, []string{"read", "write", "edit"}) || !reflect.DeepEqual(contract.FilesystemBridge.NativeTools.Disabled, []string{"apply_patch"}) || contract.FilesystemBridge.NativeTools.WriteRequiresWorkspaceAccess != "rw" {
		t.Fatalf("filesystem contract = %#v", contract.FilesystemBridge)
	}
	if contract.Secrets.TokenConfigField != "tokenFile" || contract.Secrets.TokenFile != "/run/aries/e2b/access.token" || contract.Secrets.TokenBytesInOpenClawJSON || contract.Secrets.PluginEntrySupportsSecretRef {
		t.Fatalf("secret staging = %#v", contract.Secrets)
	}
	if contract.Limitations.BuiltInE2BBackend || contract.Limitations.DirectProcessStartMethod || contract.Limitations.DirectProcessSendSignalMethod || !contract.Limitations.BuildExecSpecAcceptsCommandString || contract.Limitations.FilesystemBridgeHasListDir {
		t.Fatalf("v2026.7.1 limitations = %#v", contract.Limitations)
	}
}

func TestAriesPluginUsesOnlyPinnedBackendAndFilesystemSurface(t *testing.T) {
	index, err := os.ReadFile("assets/aries-e2b/index.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{
		[]byte(`from "openclaw/plugin-sdk/sandbox"`), []byte(`registerSandboxBackend("aries-e2b"`),
		[]byte("buildExecSpec:"), []byte("runShellCommand:"), []byte("createFsBridge:"),
		[]byte(`argv: [helperPath, "exec", command, requestedWorkdir ?? workdir, JSON.stringify(env)]`),
		[]byte(`stdinMode: "pipe-closed"`), []byte(`if (usePty)`),
	} {
		if !bytes.Contains(index, required) {
			t.Fatalf("plugin missing pinned contract %q", required)
		}
	}
	for _, forbidden := range [][]byte{[]byte("manager:"), []byte("registerSandboxBackendManager"), []byte("listDir"), []byte("createSandbox"), []byte("deleteSandbox"), []byte("ConnectRPC")} {
		if bytes.Contains(index, forbidden) {
			t.Fatalf("plugin contains forbidden lifecycle/protocol surface %q", forbidden)
		}
	}
	client, err := os.ReadFile("assets/aries-e2b/client.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"resolvePath", "readFile", "writeFile", "mkdirp", "remove", "rename", "stat"} {
		if !bytes.Contains(client, []byte(method)) {
			t.Fatalf("filesystem client missing %s", method)
		}
	}
	for _, required := range [][]byte{[]byte(`cmd: "/bin/bash"`), []byte(`args: ["-lc", command, "aries-e2b", ...args]`), []byte(`E2b-Sandbox-Id`), []byte(`X-Access-Token`)} {
		if !bytes.Contains(client, required) {
			t.Fatalf("bridge client missing %q", required)
		}
	}
	if bytes.Contains(client, []byte("listDir")) {
		t.Fatal("plugin invented a listDir method absent from pinned SandboxFsBridge")
	}
}
