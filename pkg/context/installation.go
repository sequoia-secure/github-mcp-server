package context

import "context"

type installationOwnerKey struct{}

// WithInstallationOwner records the GitHub account login (organization or
// user) whose GitHub App installation should authenticate the API requests
// made while handling the current tool call.
func WithInstallationOwner(ctx context.Context, login string) context.Context {
	return context.WithValue(ctx, installationOwnerKey{}, login)
}

// GetInstallationOwner returns the login set by WithInstallationOwner, and
// false when none was set.
func GetInstallationOwner(ctx context.Context) (string, bool) {
	login, ok := ctx.Value(installationOwnerKey{}).(string)
	return login, ok && login != ""
}
