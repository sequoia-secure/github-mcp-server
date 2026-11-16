package github

import (
	"context"
	"testing"

	"github.com/github/github-mcp-server/v2/pkg/inventory"
	"github.com/github/github-mcp-server/v2/pkg/translations"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolHandlerMiddlewareWrapsEveryRegisteredTool guards the seam that
// installation routing depends on. NewInstallationRoutingMiddleware is passed
// to the server through MCPServerConfig.ToolHandlerMiddleware; if any tool,
// typed or legacy, were registered without that chain, a call to it would
// skip routing and run against whatever token the context happened to carry.
// Upstream has moved tools between registration paths before (typed tool
// outputs), so this builds the real server with every toolset, calls each
// advertised tool, and requires the middleware to answer every call.
func TestToolHandlerMiddlewareWrapsEveryRegisteredTool(t *testing.T) {
	const sentinel = "routed-by-test-middleware"
	reached := map[string]bool{}
	middleware := func(_ mcp.ToolHandler) mcp.ToolHandler {
		return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			reached[req.Params.Name] = true
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sentinel}}}, nil
		}
	}

	cfg := MCPServerConfig{
		Version:               "test",
		Token:                 "test-token",
		EnabledToolsets:       []string{"all"},
		Translator:            translations.NullTranslationHelper,
		ContentWindowSize:     5000,
		ToolHandlerMiddleware: []inventory.ToolHandlerMiddleware{middleware},
	}
	inv, err := NewInventory(cfg.Translator).
		WithDeprecatedAliases(DeprecatedToolAliases).
		WithToolsets(cfg.EnabledToolsets).
		Build()
	require.NoError(t, err)

	server, err := NewMCPServer(context.Background(), &cfg, stubDeps{obsv: stubExporters()}, inv)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	var names []string
	for tool, err := range session.Tools(context.Background(), nil) {
		require.NoError(t, err)
		names = append(names, tool.Name)
	}
	require.NotEmpty(t, names)

	var bypassed []string
	for _, name := range names {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if !reached[name] {
			bypassed = append(bypassed, name)
		}
	}
	assert.Empty(t, bypassed, "tools registered without the server's ToolHandlerMiddleware (installation routing would be skipped)")
	t.Logf("middleware reached %d of %d advertised tools", len(names)-len(bypassed), len(names))
}
