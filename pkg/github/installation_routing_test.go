package github

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
	"github.com/github/github-mcp-server/pkg/utils"
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

func TestResultLooksEmpty(t *testing.T) {
	assert.True(t, resultLooksEmpty(``))
	assert.True(t, resultLooksEmpty(`[]`))
	assert.True(t, resultLooksEmpty(`{"total_count":0,"incomplete_results":false,"items":[]}`))
	assert.True(t, resultLooksEmpty(`{"items":[]}`))
	assert.False(t, resultLooksEmpty(`{"total_count":2,"items":[{"a":1},{"a":2}]}`))
	assert.False(t, resultLooksEmpty(`{"items":[{"a":1}]}`))
	assert.False(t, resultLooksEmpty(`{"login":"octocat"}`), "objects without a count or items are kept")
	assert.False(t, resultLooksEmpty(`not json`), "unparseable text is never dropped")
}

func routing(known ...string) InstallationRouting {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	return InstallationRouting{
		Known:  func(login string) bool { return set[login] },
		Logins: func() []string { return append([]string(nil), known...) },
	}
}

func request(tool, args string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: tool, Arguments: json.RawMessage(args)}}
}

func errorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.True(t, res.IsError)
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text
}

func TestInstallationRoutingMiddleware_ExplicitAccount(t *testing.T) {
	capture := func(got *string) mcp.ToolHandler {
		return func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			login, ok := ghcontext.GetInstallationOwner(ctx)
			require.True(t, ok, "installation owner must be set on the context")
			*got = login
			return &mcp.CallToolResult{}, nil
		}
	}

	t.Run("routes by owner, case-insensitively", func(t *testing.T) {
		var got string
		h := NewInstallationRoutingMiddleware(routing("acme", "beta"))(capture(&got))
		res, err := h(context.Background(), request("get_file_contents", `{"owner":"ACME","repo":"x"}`))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, "acme", got)
	})

	t.Run("routes by search qualifier", func(t *testing.T) {
		var got string
		h := NewInstallationRoutingMiddleware(routing("acme", "beta"))(capture(&got))
		_, err := h(context.Background(), request("search_code", `{"query":"TODO repo:Beta/thing"}`))
		require.NoError(t, err)
		assert.Equal(t, "beta", got)
	})

	t.Run("unknown account is rejected before the handler runs and lists the configured orgs", func(t *testing.T) {
		called := false
		next := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = true
			return &mcp.CallToolResult{}, nil
		}
		h := NewInstallationRoutingMiddleware(routing("acme", "beta"))(next)
		res, err := h(context.Background(), request("get_file_contents", `{"owner":"stranger"}`))
		require.NoError(t, err)
		assert.False(t, called)
		text := errorText(t, res)
		assert.Contains(t, text, `"stranger"`)
		assert.Contains(t, text, "acme, beta")
	})
}

func TestInstallationRoutingMiddleware_NoAccount(t *testing.T) {
	t.Run("installation-independent tools use the first configured installation", func(t *testing.T) {
		var got string
		next := func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			got, _ = ghcontext.GetInstallationOwner(ctx)
			return &mcp.CallToolResult{}, nil
		}
		h := NewInstallationRoutingMiddleware(routing("beta", "acme"))(next)
		_, err := h(context.Background(), request("get_me", `{}`))
		require.NoError(t, err)
		assert.Equal(t, "beta", got, "Logins() order decides; callers pass a sorted list")
	})

	t.Run("other tools are rejected with the configured orgs", func(t *testing.T) {
		called := false
		next := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = true
			return &mcp.CallToolResult{}, nil
		}
		h := NewInstallationRoutingMiddleware(routing("acme", "beta"))(next)
		res, err := h(context.Background(), request("list_branches", `{"repo":"x"}`))
		require.NoError(t, err)
		assert.False(t, called)
		assert.Contains(t, errorText(t, res), "acme, beta")
	})

	t.Run("nil request is treated as no arguments", func(t *testing.T) {
		h := NewInstallationRoutingMiddleware(routing("acme"))(func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Fatal("handler must not run")
			return nil, nil
		})
		res, err := h(context.Background(), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	t.Run("no installations configured", func(t *testing.T) {
		r := InstallationRouting{Known: func(string) bool { return false }, Logins: func() []string { return nil }}
		h := NewInstallationRoutingMiddleware(r)(func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Fatal("handler must not run")
			return nil, nil
		})
		res, err := h(context.Background(), request("search_repositories", `{"query":"x"}`))
		require.NoError(t, err)
		assert.Contains(t, errorText(t, res), "no GitHub App installations")
	})
}

func TestInstallationRoutingMiddleware_FanOut(t *testing.T) {
	// A fake search handler that answers per organization.
	perOrg := func(answers map[string]string, errs map[string]error) mcp.ToolHandler {
		return func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			login, _ := ghcontext.GetInstallationOwner(ctx)
			if err, ok := errs[login]; ok {
				return nil, err
			}
			if text, ok := answers[login]; ok {
				if text == "ERR" {
					return utils.NewToolResultError("boom in " + login), nil
				}
				return utils.NewToolResultText(text), nil
			}
			return utils.NewToolResultText(`{"total_count":0,"items":[]}`), nil
		}
	}
	texts := func(res *mcp.CallToolResult) []string {
		var out []string
		for _, c := range res.Content {
			out = append(out, c.(*mcp.TextContent).Text)
		}
		return out
	}

	t.Run("runs every installation and keeps only the ones with hits, in login order", func(t *testing.T) {
		var calls atomic.Int32
		var mu sync.Mutex
		seen := map[string]bool{}
		inner := perOrg(map[string]string{
			"gamma": `{"total_count":1,"items":[{"full_name":"gamma/thing"}]}`,
			"alpha": `{"total_count":2,"items":[{"full_name":"alpha/thing"},{"full_name":"alpha/other"}]}`,
		}, nil)
		queries := map[string]string{}
		next := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			login, _ := ghcontext.GetInstallationOwner(ctx)
			var args map[string]any
			require.NoError(t, json.Unmarshal(req.Params.Arguments, &args))
			mu.Lock()
			seen[login] = true
			queries[login] = args["query"].(string)
			mu.Unlock()
			return inner(ctx, req)
		}
		original := request("search_repositories", `{"query":"thing","per_page":5}`)
		h := NewInstallationRoutingMiddleware(routing("alpha", "beta", "gamma"))(next)
		res, err := h(context.Background(), original)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, int32(3), calls.Load(), "one call per installation")
		assert.Equal(t, map[string]bool{"alpha": true, "beta": true, "gamma": true}, seen)
		assert.Equal(t, map[string]string{
			"alpha": "thing org:alpha", "beta": "thing org:beta", "gamma": "thing org:gamma",
		}, queries, "each installation's search is confined to its organization")
		assert.JSONEq(t, `{"query":"thing","per_page":5}`, string(original.Params.Arguments), "the caller's request is not mutated")

		got := texts(res)
		require.Len(t, got, 2, "beta had no hits and is omitted")
		assert.Contains(t, got[0], "Organization alpha:")
		assert.Contains(t, got[0], "alpha/other")
		assert.Contains(t, got[1], "Organization gamma:")
		assert.Contains(t, got[1], "gamma/thing")
	})

	t.Run("no hits anywhere says so and names the orgs", func(t *testing.T) {
		h := NewInstallationRoutingMiddleware(routing("alpha", "beta"))(perOrg(nil, nil))
		res, err := h(context.Background(), request("search_code", `{"query":"needle"}`))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		got := texts(res)
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "No results in any configured organization (alpha, beta)")
	})

	t.Run("partial failures are appended after the hits and do not fail the call", func(t *testing.T) {
		h := NewInstallationRoutingMiddleware(routing("alpha", "beta", "gamma"))(perOrg(
			map[string]string{"alpha": `{"total_count":1,"items":[{"x":1}]}`, "beta": "ERR"},
			map[string]error{"gamma": errors.New("network down")},
		))
		res, err := h(context.Background(), request("search_issues", `{"query":"bug"}`))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		got := texts(res)
		require.Len(t, got, 3)
		assert.Contains(t, got[0], "Organization alpha:")
		assert.Contains(t, got[1], "Organization beta: error: boom in beta")
		assert.Contains(t, got[2], "Organization gamma: error: network down")
	})

	t.Run("all failures make the call an error", func(t *testing.T) {
		h := NewInstallationRoutingMiddleware(routing("alpha", "beta"))(perOrg(
			map[string]string{"alpha": "ERR", "beta": "ERR"}, nil))
		res, err := h(context.Background(), request("search_code", `{"query":"who"}`))
		require.NoError(t, err)
		assert.True(t, res.IsError)
		assert.Len(t, res.Content, 2)
	})

	t.Run("an explicit qualifier disables the fan-out", func(t *testing.T) {
		var calls atomic.Int32
		inner := perOrg(map[string]string{"beta": `{"total_count":1,"items":[{"x":1}]}`}, nil)
		next := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			return inner(ctx, req)
		}
		h := NewInstallationRoutingMiddleware(routing("alpha", "beta"))(next)
		res, err := h(context.Background(), request("search_repositories", `{"query":"thing org:beta"}`))
		require.NoError(t, err)
		assert.Equal(t, int32(1), calls.Load())
		got := texts(res)
		require.Len(t, got, 1)
		assert.NotContains(t, got[0], "Organization", "single-org results are passed through untouched")
	})

	t.Run("concurrency is bounded", func(t *testing.T) {
		var inFlight, peak atomic.Int32
		next := func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			defer inFlight.Add(-1)
			return utils.NewToolResultText(`{"total_count":1,"items":[{}]}`), nil
		}
		r := routing("a", "b", "c", "d", "e", "f")
		r.MaxConcurrency = 2
		h := NewInstallationRoutingMiddleware(r)(next)
		res, err := h(context.Background(), request("search_commits", `{"query":"x"}`))
		require.NoError(t, err)
		assert.Len(t, res.Content, 6)
		assert.LessOrEqual(t, peak.Load(), int32(2))
	})
}

func TestWithOrgQualifier(t *testing.T) {
	t.Run("appends to query", func(t *testing.T) {
		got := withOrgQualifier(request("search_code", `{"query":"needle language:go","per_page":3}`), "acme")
		var args map[string]any
		require.NoError(t, json.Unmarshal(got.Params.Arguments, &args))
		assert.Equal(t, "needle language:go org:acme", args["query"])
		assert.Equal(t, float64(3), args["per_page"], "other arguments are preserved")
		assert.Equal(t, "search_code", got.Params.Name)
	})
	t.Run("appends to q", func(t *testing.T) {
		got := withOrgQualifier(request("search_x", `{"q":"needle"}`), "acme")
		var args map[string]any
		require.NoError(t, json.Unmarshal(got.Params.Arguments, &args))
		assert.Equal(t, "needle org:acme", args["q"])
	})
	t.Run("no query argument returns the same request", func(t *testing.T) {
		req := request("search_x", `{"per_page":3}`)
		assert.Same(t, req, withOrgQualifier(req, "acme"))
	})
	t.Run("nil request", func(t *testing.T) {
		assert.Nil(t, withOrgQualifier(nil, "acme"))
	})
}

func TestDefaultToolSets(t *testing.T) {
	// Guard against accidental edits: these names must match the server's
	// registered tools, which the routing keys on.
	assert.ElementsMatch(t, []string{
		"search_code", "search_commits", "search_issues",
		"search_pull_requests", "search_repositories",
	}, sortedKeys(DefaultFanOutTools))
	assert.ElementsMatch(t, []string{
		"get_global_security_advisory", "get_me", "list_global_security_advisories",
		"search_orgs", "search_users",
	}, sortedKeys(DefaultAnyInstallationTools))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
