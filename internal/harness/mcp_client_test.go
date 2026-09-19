package harness

import (
	"context"
	"testing"
)

func TestValidateMCPServer(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MCPServerConfig
		wantErr bool
	}{
		{
			name: "valid command with args",
			cfg: MCPServerConfig{
				Name:    "filesystem",
				Command: "npx",
				Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "/workspace"},
			},
			wantErr: false,
		},
		{
			name: "valid url",
			cfg: MCPServerConfig{
				Name: "remote-sse",
				URL:  "https://mcp.example.com/sse",
			},
			wantErr: false,
		},
		{
			name: "missing name",
			cfg: MCPServerConfig{
				Command: "ls",
			},
			wantErr: true,
		},
		{
			name: "invalid name characters",
			cfg: MCPServerConfig{
				Name:    "invalid name with spaces",
				Command: "ls",
			},
			wantErr: true,
		},
		{
			name: "both command and url",
			cfg: MCPServerConfig{
				Name:    "both",
				Command: "ls",
				URL:     "https://mcp.example.com/sse",
			},
			wantErr: true,
		},
		{
			name: "neither command nor url",
			cfg: MCPServerConfig{
				Name: "empty",
			},
			wantErr: true,
		},
		{
			name: "control character in command",
			cfg: MCPServerConfig{
				Name:    "bad-cmd",
				Command: "ls\x00",
			},
			wantErr: true,
		},
		{
			name: "control character in args",
			cfg: MCPServerConfig{
				Name:    "bad-arg",
				Command: "ls",
				Args:    []string{"-la\n"},
			},
			wantErr: true,
		},
		{
			name: "invalid url scheme",
			cfg: MCPServerConfig{
				Name: "bad-url",
				URL:  "ftp://mcp.example.com",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMCPServer(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateMCPServer() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewMCPClient_Validation(t *testing.T) {
	_, err := NewMCPClient(MCPServerConfig{Name: ""})
	if err == nil {
		t.Fatal("NewMCPClient() expected error for empty name, got nil")
	}

	client, err := NewMCPClient(MCPServerConfig{
		Name:    "test-cmd",
		Command: "go",
		Args:    []string{"version"},
	})
	if err != nil {
		t.Fatalf("NewMCPClient() unexpected error: %v", err)
	}
	if client.Config().Name != "test-cmd" {
		t.Fatalf("client.Config().Name = %q, want %q", client.Config().Name, "test-cmd")
	}
}

func TestMCPClient_Lifecycle_UnstartedGuards(t *testing.T) {
	client, err := NewMCPClient(MCPServerConfig{
		Name:    "test-cmd",
		Command: "go",
		Args:    []string{"version"},
	})
	if err != nil {
		t.Fatalf("NewMCPClient() unexpected error: %v", err)
	}

	ctx := context.Background()

	// Calling FetchAndMapTools before start must return error
	_, err = client.FetchAndMapTools(ctx)
	if err == nil {
		t.Fatal("FetchAndMapTools() expected error before start, got nil")
	}

	// Calling CallTool before start must return error
	_, err = client.CallTool(ctx, "any_tool", nil)
	if err == nil {
		t.Fatal("CallTool() expected error before start, got nil")
	}

	// Stop unstarted client must succeed idempotently
	if err := client.Stop(); err != nil {
		t.Fatalf("Stop() unexpected error on unstarted client: %v", err)
	}
}
