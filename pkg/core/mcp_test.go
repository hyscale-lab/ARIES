package core

import (
	"strings"
	"testing"
)

func TestValidateMCPServer(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MCPServerConfig
		wantErr string
	}{
		{
			name: "valid command server",
			cfg: MCPServerConfig{
				Name:    "fetch",
				Command: "uvx",
				Args:    []string{"mcp-server-fetch"},
			},
		},
		{
			name: "valid url server",
			cfg: MCPServerConfig{
				Name: "remote-sse",
				URL:  "https://mcp.example.com/sse",
			},
		},
		{
			name: "valid url server with http",
			cfg: MCPServerConfig{
				Name: "local-sse",
				URL:  "http://127.0.0.1:8000/sse",
			},
		},
		{
			name: "missing name",
			cfg: MCPServerConfig{
				Command: "uvx",
			},
			wantErr: "MCP server name is required",
		},
		{
			name: "invalid name characters",
			cfg: MCPServerConfig{
				Name:    "bad server name!",
				Command: "uvx",
			},
			wantErr: "contains invalid characters",
		},
		{
			name: "neither command nor url",
			cfg: MCPServerConfig{
				Name: "noop",
			},
			wantErr: "must specify either command or url",
		},
		{
			name: "both command and url",
			cfg: MCPServerConfig{
				Name:    "both",
				Command: "uvx",
				URL:     "https://mcp.example.com/sse",
			},
			wantErr: "cannot specify both command and url",
		},
		{
			name: "command with control characters",
			cfg: MCPServerConfig{
				Name:    "ctrl-cmd",
				Command: "uvx\x00malicious",
			},
			wantErr: "command contains invalid control characters",
		},
		{
			name: "argument with control characters",
			cfg: MCPServerConfig{
				Name:    "ctrl-arg",
				Command: "uvx",
				Args:    []string{"safe", "arg\nbreak"},
			},
			wantErr: "argument contains invalid control characters",
		},
		{
			name: "invalid url scheme",
			cfg: MCPServerConfig{
				Name: "ftp-server",
				URL:  "ftp://mcp.example.com/sse",
			},
			wantErr: "url must be absolute HTTP(S)",
		},
		{
			name: "valid command server with plain env and secret env",
			cfg: MCPServerConfig{
				Name:      "fetch",
				Command:   "uvx",
				Args:      []string{"mcp-server-fetch"},
				Env:       map[string]string{"DEBUG": "1", "PORT": "8080"},
				SecretEnv: map[string]string{"API_KEY": "TOOLATHLON_API_KEY"},
			},
		},
		{
			name: "invalid env key name",
			cfg: MCPServerConfig{
				Name:    "env-server",
				Command: "uvx",
				Env:     map[string]string{"BAD-KEY": "SAFE_VAL"},
			},
			wantErr: "is not a valid environment variable name",
		},
		{
			name: "env value with control characters",
			cfg: MCPServerConfig{
				Name:    "env-server",
				Command: "uvx",
				Env:     map[string]string{"KEY": "val\x00bad"},
			},
			wantErr: "contains invalid control characters",
		},
		{
			name: "invalid secret_env key name",
			cfg: MCPServerConfig{
				Name:      "env-server",
				Command:   "uvx",
				SecretEnv: map[string]string{"BAD-KEY": "SAFE_VAL"},
			},
			wantErr: "is not a valid environment variable name",
		},
		{
			name: "invalid secret_env host variable name (raw secret)",
			cfg: MCPServerConfig{
				Name:      "env-server",
				Command:   "uvx",
				SecretEnv: map[string]string{"API_KEY": "sk-proj-raw-secret!"},
			},
			wantErr: "is not a valid host environment variable name",
		},
		{
			name: "key collision between env and secret_env",
			cfg: MCPServerConfig{
				Name:      "env-server",
				Command:   "uvx",
				Env:       map[string]string{"DUPLICATE": "plain"},
				SecretEnv: map[string]string{"DUPLICATE": "HOST_VAR"},
			},
			wantErr: "cannot be specified in both env and secret_env",
		},
		{
			name: "env rejected for url server",
			cfg: MCPServerConfig{
				Name: "remote-sse",
				URL:  "https://mcp.example.com/sse",
				Env:  map[string]string{"DEBUG": "1"},
			},
			wantErr: "env is not supported for url servers",
		},
		{
			name: "secret_env rejected for url server",
			cfg: MCPServerConfig{
				Name:      "remote-sse",
				URL:       "https://mcp.example.com/sse",
				SecretEnv: map[string]string{"API_KEY": "TOOLATHLON_API_KEY"},
			},
			wantErr: "secret_env is not supported for url servers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMCPServer(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateMCPServer() unexpected error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateMCPServer() expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateMCPServer() error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}
