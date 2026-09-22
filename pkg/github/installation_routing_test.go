package github

import (
	"context"
	"encoding/json"
	"testing"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnerFromArguments(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{"owner", `{"owner":"Acme","repo":"widgets"}`, "Acme"},
		{"org", `{"org":"acme"}`, "acme"},
		{"organization", `{"organization":"acme"}`, "acme"},
		{"username", `{"username":"octocat"}`, "octocat"},
		{"user", `{"user":"octocat"}`, "octocat"},
		{"login", `{"login":"octocat"}`, "octocat"},
		{"owner wins over query", `{"owner":"acme","query":"org:other"}`, "acme"},
		{"repository owner/name", `{"repository":"acme/widgets"}`, "acme"},
		{"repository bare name is not an owner", `{"repository":"widgets"}`, ""},
		{"query org qualifier", `{"query":"needle org:acme language:go"}`, "acme"},
		{"query repo qualifier", `{"query":"repo:acme/widgets needle"}`, "acme"},
		{"query user qualifier", `{"query":"user:octocat"}`, "octocat"},
		{"q alias", `{"q":"fix panic org:acme"}`, "acme"},
		{"qualifier must be token-initial", `{"query":"xorg:acme"}`, ""},
		{"unqualified query", `{"query":"needle language:go"}`, ""},
		{"empty owner ignored", `{"owner":"  ","org":"acme"}`, "acme"},
		{"non-string owner ignored", `{"owner":42}`, ""},
		{"no args", ``, ""},
		{"invalid json", `{`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ownerFromArguments(json.RawMessage(tt.args)))
		})
	}
}

func TestInstallationRoutingMiddleware(t *testing.T) {
	known := func(login string) bool { return login == "acme" || login == "beta" }

	capture := func(got *string) mcp.ToolHandler {
		return func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			login, ok := ghcontext.GetInstallationOwner(ctx)
			require.True(t, ok, "installation owner must be set on the context")
			*got = login
			return &mcp.CallToolResult{}, nil
		}
	}
	request := func(args string) *mcp.CallToolRequest {
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(args)}}
	}

	t.Run("routes by owner, case-insensitively", func(t *testing.T) {
		var got string
		h := NewInstallationRoutingMiddleware(known, "")(capture(&got))
		res, err := h(context.Background(), request(`{"owner":"ACME","repo":"x"}`))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, "acme", got)
	})

	t.Run("routes by search qualifier", func(t *testing.T) {
		var got string
		h := NewInstallationRoutingMiddleware(known, "")(capture(&got))
		_, err := h(context.Background(), request(`{"query":"TODO repo:Beta/thing"}`))
		require.NoError(t, err)
		assert.Equal(t, "beta", got)
	})

	t.Run("falls back to the default installation", func(t *testing.T) {
		var got string
		h := NewInstallationRoutingMiddleware(known, "Acme")(capture(&got))
		_, err := h(context.Background(), request(`{"query":"needle"}`))
		require.NoError(t, err)
		assert.Equal(t, "acme", got)
	})

	t.Run("nil request uses the default", func(t *testing.T) {
		var got string
		h := NewInstallationRoutingMiddleware(known, "acme")(capture(&got))
		_, err := h(context.Background(), nil)
		require.NoError(t, err)
		assert.Equal(t, "acme", got)
	})

	t.Run("unknown account is rejected before the handler runs", func(t *testing.T) {
		called := false
		next := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = true
			return &mcp.CallToolResult{}, nil
		}
		h := NewInstallationRoutingMiddleware(known, "acme")(next)
		res, err := h(context.Background(), request(`{"owner":"stranger"}`))
		require.NoError(t, err)
		assert.False(t, called)
		require.True(t, res.IsError)
		text, ok := res.Content[0].(*mcp.TextContent)
		require.True(t, ok)
		assert.Contains(t, text.Text, `"stranger"`)
	})

	t.Run("no account and no default is rejected", func(t *testing.T) {
		called := false
		next := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = true
			return &mcp.CallToolResult{}, nil
		}
		h := NewInstallationRoutingMiddleware(known, "")(next)
		res, err := h(context.Background(), request(`{}`))
		require.NoError(t, err)
		assert.False(t, called)
		require.True(t, res.IsError)
	})
}
