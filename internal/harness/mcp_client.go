package harness

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/client"
	"github.com/modelcontextprotocol/go-sdk/transport/stdio"
)

// MCPClient handles the lifecycle of an MCP client using stdio.
type MCPClient struct {
	client client.Client
}

// NewMCPClient creates a new MCP client instance.
func NewMCPClient() (*MCPClient, error) {
	// Initialize stdio transport for the MCP client
	transport := stdio.NewTransport()

	// Create new MCP client with the stdio transport
	c := client.New(transport)

	return &MCPClient{
		client: c,
	}, nil
}

// Start connects the client.
func (m *MCPClient) Start(ctx context.Context) error {
	if err := m.client.Connect(ctx); err != nil {
		return fmt.Errorf("failed to start MCP client: %w", err)
	}
	return nil
}

// Stop closes the client connection.
func (m *MCPClient) Stop() error {
	return m.client.Close()
}

// FetchAndMapTools fetches tools from the MCP server and maps them to Hermes schema.
func (m *MCPClient) FetchAndMapTools(ctx context.Context) ([]map[string]interface{}, error) {
	// Fetch tools using the standard MCP tool fetching signature.
	res, err := m.client.ListTools(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list MCP tools: %w", err)
	}

	var mappedTools []map[string]interface{}
	for _, tool := range res.Tools {
		mappedTools = append(mappedTools, map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.InputSchema,
		})
	}
	return mappedTools, nil
}
