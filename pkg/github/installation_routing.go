package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ownerArgumentKeys are the tool arguments that name the account a call is
// about, in precedence order. "owner" and "org" cover almost every tool;
// the others cover user-profile and team tools.
var ownerArgumentKeys = []string{"owner", "org", "organization", "username", "user", "login"}

// queryArgumentKeys are free-text search arguments that may carry an
// account through a qualifier such as org:acme or repo:acme/widgets.
var queryArgumentKeys = []string{"query", "q"}

// searchQualifier matches the qualifiers that scope a GitHub search to an
// account. Logins are alphanumerics and single hyphens, up to 39 chars.
var searchQualifier = regexp.MustCompile(`(?i)(?:^|\s)(?:org|user|owner|repo):([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))`)

// DefaultFanOutTools are the search tools that, when called without an
// account, run against every configured installation concurrently and return
// the results grouped by organization. GitHub's search API is not scoped to
// the authenticating installation (an installation token can search public
// repositories anywhere), so each fan-out call has "org:<login>" appended to
// its query to confine it to that organization. They are the discovery tools:
// a caller who does not know which organization owns a repository finds it
// here and then addresses that organization directly in later calls.
var DefaultFanOutTools = map[string]bool{
	"search_repositories":  true,
	"search_code":          true,
	"search_issues":        true,
	"search_pull_requests": true,
	"search_commits":       true,
}

// DefaultAnyInstallationTools are the tools whose result does not depend on
// which installation authenticates them. They run against the first
// configured installation. User and organization search are here because
// GitHub offers no organization qualifier for them, so a fan-out would return
// the same results once per installation.
var DefaultAnyInstallationTools = map[string]bool{
	"get_me":                          true,
	"list_global_security_advisories": true,
	"get_global_security_advisory":    true,
	"search_users":                    true,
	"search_orgs":                     true,
}

// InstallationRouting configures NewInstallationRoutingMiddleware.
type InstallationRouting struct {
	// Known reports whether an installation is configured for login.
	Known func(login string) bool

	// Logins returns the configured account logins, sorted.
	Logins func() []string

	// MaxConcurrency bounds the parallel calls of a fan-out. Defaults to 8.
	MaxConcurrency int
}

// NewInstallationRoutingMiddleware returns a tool-handler middleware that
// decides which GitHub App installation authenticates each tool call and
// records it in the request context, where transport.BearerAuthTransport
// picks it up through the TokenProviderCtx hook (see githubapp.MultiProvider).
//
// The account is read from the tool's arguments. A call that names an
// account with no configured installation is rejected before any API
// request is made, with a message listing the configured organizations, so
// the caller sees a clear error instead of a misleading 404 from GitHub.
//
// A call that names no account is handled by tool:
//   - discovery tools (FanOutTools) run against every installation in
//     parallel, each with "org:<login>" appended to the query, and return one
//     result block per organization that had matches;
//   - installation-independent tools (AnyInstallationTools) run against the
//     first configured installation;
//   - anything else is rejected with the list of configured organizations.
func NewInstallationRoutingMiddleware(r InstallationRouting) inventory.ToolHandlerMiddleware {
	fanOut, anyInst := DefaultFanOutTools, DefaultAnyInstallationTools
	maxConc := r.MaxConcurrency
	if maxConc <= 0 {
		maxConc = 8
	}

	return func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var raw json.RawMessage
			var toolName string
			if req != nil && req.Params != nil {
				raw = req.Params.Arguments
				toolName = req.Params.Name
			}

			if login := strings.ToLower(ownerFromArguments(raw)); login != "" {
				if !r.Known(login) {
					return utils.NewToolResultError(fmt.Sprintf(
						"GitHub account %q has no configured App installation on this server; configured organizations: %s",
						login, strings.Join(r.Logins(), ", "))), nil
				}
				return next(ghcontext.WithInstallationOwner(ctx, login), req)
			}

			logins := r.Logins()
			if len(logins) == 0 {
				return utils.NewToolResultError("no GitHub App installations are configured on this server"), nil
			}

			switch {
			case anyInst[toolName]:
				return next(ghcontext.WithInstallationOwner(ctx, logins[0]), req)
			case fanOut[toolName]:
				return fanOutAcrossInstallations(ctx, next, req, logins, maxConc), nil
			default:
				return utils.NewToolResultError(fmt.Sprintf(
					"this call does not identify a GitHub organization; pass owner/org (or an org:/repo: search qualifier). Configured organizations: %s",
					strings.Join(logins, ", "))), nil
			}
		}
	}
}

// fanOutAcrossInstallations runs the call once per installation, in
// parallel, with the search query confined to that installation's
// organization, and merges the outcomes into a single result grouped by
// organization. Organizations with no matches are omitted so the caller
// sees only where something was found.
func fanOutAcrossInstallations(ctx context.Context, next mcp.ToolHandler, req *mcp.CallToolRequest, logins []string, maxConc int) *mcp.CallToolResult {
	type outcome struct {
		res *mcp.CallToolResult
		err error
	}
	outcomes := make([]outcome, len(logins))

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConc)
	for i, login := range logins {
		wg.Add(1)
		go func(i int, login string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := next(ghcontext.WithInstallationOwner(ctx, login), withOrgQualifier(req, login))
			outcomes[i] = outcome{res: res, err: err}
		}(i, login)
	}
	wg.Wait()

	var found, failed []mcp.Content
	for i, login := range logins {
		o := outcomes[i]
		switch {
		case o.err != nil:
			failed = append(failed, &mcp.TextContent{Text: fmt.Sprintf("Organization %s: error: %v", login, o.err)})
		case o.res == nil:
			failed = append(failed, &mcp.TextContent{Text: fmt.Sprintf("Organization %s: error: no result", login)})
		case o.res.IsError:
			failed = append(failed, &mcp.TextContent{Text: fmt.Sprintf("Organization %s: error: %s", login, textOf(o.res.Content))})
		default:
			text := textOf(o.res.Content)
			if resultLooksEmpty(text) {
				continue
			}
			found = append(found, &mcp.TextContent{Text: fmt.Sprintf("Organization %s:\n%s", login, text)})
		}
	}

	switch {
	case len(found) > 0:
		// Partial failures are reported after the hits so the caller can
		// still act on what was found.
		return &mcp.CallToolResult{Content: append(found, failed...)}
	case len(failed) > 0:
		return &mcp.CallToolResult{IsError: true, Content: failed}
	default:
		return utils.NewToolResultText(fmt.Sprintf(
			"No results in any configured organization (%s).", strings.Join(logins, ", ")))
	}
}

// withOrgQualifier returns a shallow copy of req whose search argument
// ("query" or "q") has " org:<login>" appended, so the search is evaluated
// within that organization. Requests without a string search argument are
// returned unchanged.
func withOrgQualifier(req *mcp.CallToolRequest, login string) *mcp.CallToolRequest {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return req
	}
	var args map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return req
	}
	rewritten := false
	for _, key := range queryArgumentKeys {
		if q, ok := args[key].(string); ok {
			args[key] = strings.TrimSpace(q + " org:" + login)
			rewritten = true
			break
		}
	}
	if !rewritten {
		return req
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return req
	}
	params := *req.Params
	params.Arguments = raw
	clone := *req
	clone.Params = &params
	return &clone
}

// textOf concatenates the text blocks of a result.
func textOf(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if t, ok := c.(*mcp.TextContent); ok && t.Text != "" {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// resultLooksEmpty reports whether a JSON tool result carries no items: a
// search envelope with total_count 0 or an empty items list, or a bare
// empty array. Anything unparseable is treated as non-empty so nothing is
// silently dropped.
func resultLooksEmpty(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return false
	}
	switch val := v.(type) {
	case []any:
		return len(val) == 0
	case map[string]any:
		if tc, ok := val["total_count"].(float64); ok {
			return tc == 0
		}
		if items, ok := val["items"].([]any); ok {
			return len(items) == 0
		}
	}
	return false
}

// ownerFromArguments extracts the account login a tool call is about, or ""
// when the arguments name none.
func ownerFromArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	for _, key := range ownerArgumentKeys {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	// A "repository" argument of the form owner/name.
	if v, ok := args["repository"].(string); ok {
		if owner, _, found := strings.Cut(strings.TrimSpace(v), "/"); found && owner != "" {
			return owner
		}
	}
	for _, key := range queryArgumentKeys {
		if v, ok := args[key].(string); ok {
			if m := searchQualifier.FindStringSubmatch(v); m != nil {
				return m[1]
			}
		}
	}
	return ""
}
