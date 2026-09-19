package harness

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServerConfig defines the configuration for an external or in-harness MCP server.
type MCPServerConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// ValidateMCPServer verifies that an MCP server configuration specifies a valid name
// and either a validated executable command or an absolute HTTP(S) URL.
func ValidateMCPServer(cfg MCPServerConfig) error {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return errors.New("MCP server name is required")
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("MCP server name %q contains invalid characters (must be alphanumeric, '-', or '_')", name)
	}

	hasCommand := strings.TrimSpace(cfg.Command) != ""
	hasURL := strings.TrimSpace(cfg.URL) != ""

	if !hasCommand && !hasURL {
		return fmt.Errorf("MCP server %q must specify either command or url", name)
	}
	if hasCommand && hasURL {
		return fmt.Errorf("MCP server %q cannot specify both command and url", name)
	}

	if hasCommand {
		cmd := strings.TrimSpace(cfg.Command)
		if strings.ContainsFunc(cmd, unicode.IsControl) {
			return fmt.Errorf("MCP server %q command contains invalid control characters", name)
		}
		for _, arg := range cfg.Args {
			if strings.ContainsFunc(arg, unicode.IsControl) {
				return fmt.Errorf("MCP server %q argument contains invalid control characters", name)
			}
		}
	}

	if hasURL {
		parsed, err := url.Parse(cfg.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("MCP server %q url must be absolute HTTP(S)", name)
		}
	}

	return nil
}

// MCPClient manages the live session and lifecycle of an MCP client adapter.
type MCPClient struct {
	mu        sync.Mutex
	cfg       MCPServerConfig
	client    *mcp.Client
	session   *mcp.ClientSession
	transport mcp.Transport
	closed    bool
}

// NewMCPClient creates a new MCP client instance with validated transport configuration.
func NewMCPClient(cfg MCPServerConfig) (*MCPClient, error) {
	if err := ValidateMCPServer(cfg); err != nil {
		return nil, fmt.Errorf("validate MCP server config: %w", err)
	}

	var transport mcp.Transport
	if cfg.Command != "" {
		cmd := exec.Command(cfg.Command, cfg.Args...)
		if len(cfg.Env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range cfg.Env {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
			}
		}
		transport = &mcp.CommandTransport{Command: cmd}
	} else if cfg.URL != "" {
		transport = &mcp.SSEClientTransport{Endpoint: cfg.URL}
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "aries-harness",
		Version: "1.0.0",
	}, nil)

	return &MCPClient{
		cfg:       cfg,
		client:    client,
		transport: transport,
	}, nil
}

// Config returns the configuration of this MCP client.
func (m *MCPClient) Config() MCPServerConfig {
	return m.cfg
}

// Start connects the MCP client session to the configured transport.
func (m *MCPClient) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.session != nil {
		return errors.New("MCP client session is already active")
	}
	if m.transport == nil {
		return errors.New("MCP client transport is not configured")
	}

	session, err := m.client.Connect(ctx, m.transport, nil)
	if err != nil {
		return fmt.Errorf("failed to connect MCP client session: %w", err)
	}
	m.session = session
	m.closed = false
	return nil
}

// Stop idempotently closes the active client connection and releases session resources.
func (m *MCPClient) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed || m.session == nil {
		m.closed = true
		m.session = nil
		return nil
	}

	err := m.session.Close()
	m.session = nil
	m.closed = true
	if err != nil {
		return fmt.Errorf("failed to close MCP client session: %w", err)
	}
	return nil
}

// FetchAndMapTools retrieves tools from the active MCP server and maps them into
// standard schema maps suitable for harness configuration rendering.
func (m *MCPClient) FetchAndMapTools(ctx context.Context) ([]map[string]interface{}, error) {
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()

	if session == nil {
		return nil, errors.New("MCP client session is not active")
	}

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list MCP tools: %w", err)
	}

	var mappedTools []map[string]interface{}
	for _, tool := range res.Tools {
		mapped := map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
		}
		if tool.InputSchema != nil {
			mapped["parameters"] = tool.InputSchema
		}
		mappedTools = append(mappedTools, mapped)
	}
	return mappedTools, nil
}

// CallTool forwards a runtime tool execution call directly to the live MCP session.
func (m *MCPClient) CallTool(ctx context.Context, name string, arguments map[string]any) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()

	if session == nil {
		return nil, errors.New("MCP client session is not active")
	}

	params := &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	}

	result, err := session.CallTool(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("MCP tool call %q failed: %w", name, err)
	}
	return result, nil
}
