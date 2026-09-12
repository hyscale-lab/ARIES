package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const toolathlonBenchmark = `"benchmark":{"type":"toolathlon","root":".cache/toolathlon","tasks":["canvas-list-test"],"environment":{"image":"docker.io/lockon0927/toolathlon-task-image:1016beta"}}`

const toolathlonVersions = `,"toolathlon":{"repository_url":"https://example.invalid/toolathlon.git","revision":"3333333333333333333333333333333333333333"}`

func toolathlonConfig(harness string) string {
	profile := strings.Replace(validConfig, `"benchmark":{"type":"terminalbench2","root":".cache/tb2","tasks":["fix-git"]}`, toolathlonBenchmark, 1)
	profile = strings.Replace(profile, `"harness":{"type":"openclaw"}`, harness, 1)
	return strings.Replace(profile, `"bridge":{"type":"openclaw-ssh"}`, `"bridge":{"type":"hermes-ssh"}`, 1)
}

const hermesWithGateway = `"harness":{"type":"hermes","mcp":{"servers":[{"name":"toolathlon","url":"http://task-sandbox:10086/sse","transport":"sse","timeout_seconds":300}]}}`

func TestHarnessMCPServersDecodeAndDefaultTransport(t *testing.T) {
	cfg, err := Decode(strings.NewReader(toolathlonConfig(hermesWithGateway)))
	if err != nil {
		t.Fatal(err)
	}
	servers := cfg.Harness.MCP.Servers
	if len(servers) != 1 || servers[0].Name != "toolathlon" || servers[0].Transport != "sse" || servers[0].TimeoutSeconds != 300 {
		t.Fatalf("mcp servers = %#v", servers)
	}

	defaulted := strings.Replace(hermesWithGateway, `,"transport":"sse"`, ``, 1)
	cfg, err = Decode(strings.NewReader(toolathlonConfig(defaulted)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.MCP.Servers[0].Transport != "streamable-http" {
		t.Fatalf("transport = %q, want streamable-http default", cfg.Harness.MCP.Servers[0].Transport)
	}
}

func TestHarnessMCPServersValidation(t *testing.T) {
	cases := map[string]struct {
		harness string
		wantErr string
	}{
		"openclaw": {
			harness: strings.Replace(hermesWithGateway, `"type":"hermes"`, `"type":"openclaw"`, 1),
			wantErr: "harness.mcp requires Hermes",
		},
		"bad name": {
			harness: strings.Replace(hermesWithGateway, `"name":"toolathlon"`, `"name":"Tool-athlon"`, 1),
			wantErr: "name must be a lowercase identifier",
		},
		"bad url": {
			harness: strings.Replace(hermesWithGateway, `"url":"http://task-sandbox:10086/sse"`, `"url":"task-sandbox:10086/sse"`, 1),
			wantErr: "url must be an absolute HTTP(S) URL",
		},
		"bad transport": {
			harness: strings.Replace(hermesWithGateway, `"transport":"sse"`, `"transport":"stdio"`, 1),
			wantErr: "transport must be sse or streamable-http",
		},
		"negative timeout": {
			harness: strings.Replace(hermesWithGateway, `"timeout_seconds":300`, `"timeout_seconds":-1`, 1),
			wantErr: "timeout_seconds must not be negative",
		},
		"duplicate name": {
			harness: strings.Replace(hermesWithGateway, `}]}}`, `},{"name":"toolathlon","url":"http://task-sandbox:10087/sse"}]}}`, 1),
			wantErr: "is duplicated",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			input := toolathlonConfig(testCase.harness)
			if name == "openclaw" {
				input = strings.Replace(input, `"bridge":{"type":"hermes-ssh"}`, `"bridge":{"type":"openclaw-ssh"}`, 1)
			}
			_, err := Decode(strings.NewReader(input))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), testCase.wantErr)
			}
		})
	}
}

func TestToolathlonBenchmarkValidation(t *testing.T) {
	valid := toolathlonConfig(hermesWithGateway)
	cfg, err := Decode(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Benchmark.Toolathlon != nil {
		t.Fatalf("toolathlon block = %#v, want absent by default", cfg.Benchmark.Toolathlon)
	}
	tuned := strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"gateway_port":10086,"app_host":"10.148.0.5","max_steps":50},`, 1)
	cfg, err = Decode(strings.NewReader(tuned))
	if err != nil {
		t.Fatal(err)
	}
	if settings := cfg.Benchmark.Toolathlon; settings == nil || settings.GatewayPort != 10086 || settings.AppHost != "10.148.0.5" || settings.MaxSteps != 50 {
		t.Fatalf("toolathlon block = %#v", cfg.Benchmark.Toolathlon)
	}

	cases := map[string]struct {
		input   string
		wantErr string
	}{
		"missing image": {
			input:   strings.Replace(valid, `,"environment":{"image":"docker.io/lockon0927/toolathlon-task-image:1016beta"}`, ``, 1),
			wantErr: "benchmark.environment.image is required for toolathlon",
		},
		"judge set": {
			input:   strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"judge":{"provider":"openai","base_url":"https://api.openai.com/v1","model":"gpt-4.1","api_key_env":"OPENAI_API_KEY"},`, 1),
			wantErr: "judge must not be set for toolathlon",
		},
		"gateway port too low": {
			input:   strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"gateway_port":80},`, 1),
			wantErr: "gateway_port must be between 1024 and 65535",
		},
		"negative max steps": {
			input:   strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"max_steps":-5},`, 1),
			wantErr: "max_steps must be positive",
		},
		"bad app host": {
			input:   strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"app_host":"host name"},`, 1),
			wantErr: "app_host must be a hostname or IP address",
		},
		"bracketed app host": {
			input:   strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"app_host":"[fd00::5]"},`, 1),
			wantErr: "app_host must be a hostname or IP address",
		},
		"empty host label": {
			input:   strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"app_host":"host..example"},`, 1),
			wantErr: "app_host must be a hostname or IP address",
		},
		"hyphen-edged host label": {
			input:   strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"app_host":"host-.example"},`, 1),
			wantErr: "app_host must be a hostname or IP address",
		},
		"toolathlon block under terminalbench2": {
			input:   strings.Replace(validConfig, `"tasks":["fix-git"]}`, `"tasks":["fix-git"],"toolathlon":{"max_steps":5}}`, 1),
			wantErr: "benchmark.toolathlon must not be set for terminalbench2",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(testCase.input))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), testCase.wantErr)
			}
		})
	}
	for _, host := range []string{"fd00::5", "2001:db8::1", "::1", "docker-host.internal", "10.148.0.5"} {
		literal := strings.Replace(valid, `"tasks":["canvas-list-test"],`, `"tasks":["canvas-list-test"],"toolathlon":{"app_host":"`+host+`"},`, 1)
		cfg, err := Decode(strings.NewReader(literal))
		if err != nil || cfg.Benchmark.Toolathlon == nil || cfg.Benchmark.Toolathlon.AppHost != host {
			t.Fatalf("IPv6 app host %q: %v", host, err)
		}
	}
}

// The Toolathlon pin is optional in the catalog, like the Hermes image, but a
// profile that selects the benchmark cannot load without it.
func TestToolathlonVersionPinRequiredOnlyWhenSelected(t *testing.T) {
	if _, err := DecodeVersions(strings.NewReader(validVersions)); err != nil {
		t.Fatalf("catalog without toolathlon: %v", err)
	}
	withPin := strings.Replace(validVersions, `,"openclaw":`, toolathlonVersions+`,"openclaw":`, 1)
	if _, err := DecodeVersions(strings.NewReader(withPin)); err != nil {
		t.Fatalf("catalog with toolathlon: %v", err)
	}
	partial := strings.Replace(withPin, `"revision":"3333333333333333333333333333333333333333"`, `"revision":""`, 1)
	if _, err := DecodeVersions(strings.NewReader(partial)); err == nil || !strings.Contains(err.Error(), "toolathlon") {
		t.Fatalf("partial toolathlon pin: err=%v", err)
	}

	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	configs := filepath.Join(root, "configs")
	for _, dir := range []string{profiles, configs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profilePath := filepath.Join(profiles, "profile.json")
	if err := os.WriteFile(profilePath, []byte(toolathlonConfig(hermesWithGateway)), 0o600); err != nil {
		t.Fatal(err)
	}
	versionsPath := filepath.Join(configs, "versions.json")
	if err := os.WriteFile(versionsPath, []byte(validVersions), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(profilePath); err == nil || !strings.Contains(err.Error(), "toolathlon") {
		t.Fatalf("toolathlon profile without a pin: err=%v", err)
	}
	if err := os.WriteFile(versionsPath, []byte(withPin), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Versions.Toolathlon.Revision != "3333333333333333333333333333333333333333" {
		t.Fatalf("toolathlon pin = %#v", cfg.Versions.Toolathlon)
	}
}
