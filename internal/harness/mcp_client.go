package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPClient manages the live session and lifecycle of an MCP client adapter.
type MCPClient struct {
	mu        sync.Mutex
	client    *mcp.Client
	session   *mcp.ClientSession
	transport mcp.Transport
	closed    bool
}

// NewMCPClient creates a new MCP client instance with standard client metadata.
func NewMCPClient() (*MCPClient, error) {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "aries-harness",
		Version: "1.0.0",
	}, nil)

	return &MCPClient{
		client: client,
	}, nil
}

// Start connects the MCP client session to the configured transport.
func (m *MCPClient) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.session != nil {
		return errors.New("MCP client session is already active")
	}

	if m.transport == nil {
		// Default to in-process or stdio command transport if configured
		// When no external transport is supplied, Start initializes the client readiness.
		return nil
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
		return nil, nil
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
