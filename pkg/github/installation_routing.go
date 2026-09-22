package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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

// NewInstallationRoutingMiddleware returns a tool-handler middleware that
// records, in the request context, the account whose GitHub App installation
// should authenticate the call. The account is read from the tool's
// arguments; calls that name no account use defaultLogin. Calls naming an
// account with no configured installation are rejected before any API
// request is made, so the caller sees a clear error instead of a misleading
// 404 from GitHub.
//
// The context value is consumed by transport.BearerAuthTransport through the
// TokenProviderCtx hook (see githubapp.MultiProvider).
func NewInstallationRoutingMiddleware(known func(login string) bool, defaultLogin string) inventory.ToolHandlerMiddleware {
	defaultLogin = strings.ToLower(strings.TrimSpace(defaultLogin))
	return func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var raw json.RawMessage
			if req != nil && req.Params != nil {
				raw = req.Params.Arguments
			}
			login := strings.ToLower(ownerFromArguments(raw))
			if login == "" {
				login = defaultLogin
			}
			if login == "" {
				return utils.NewToolResultError(
					"this call does not identify a GitHub organization and no default installation is configured; " +
						"pass owner/org (or an org:/repo: search qualifier)"), nil
			}
			if !known(login) {
				return utils.NewToolResultError(fmt.Sprintf(
					"GitHub account %q has no configured App installation on this server", login)), nil
			}
			return next(ghcontext.WithInstallationOwner(ctx, login), req)
		}
	}
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
